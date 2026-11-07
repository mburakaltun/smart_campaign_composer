package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"campaigncomposer/internal/config"
	"campaigncomposer/internal/httpapi"
	"campaigncomposer/internal/llm"
	"campaigncomposer/internal/metrics"
	"campaigncomposer/internal/provider"
	"campaigncomposer/internal/ratelimit"
	"campaigncomposer/internal/sender"
	"campaigncomposer/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log)
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	st := store.New(pool)
	if err := st.Migrate(ctx); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	n, err := st.SeedIfEmpty(ctx, cfg.SeedUsers)
	if err != nil {
		log.Error("seed", "err", err)
		os.Exit(1)
	}
	log.Info("database ready", "users", n)

	var client llm.Client
	if cfg.LLMProvider == "anthropic" {
		client = llm.NewAnthropic(cfg.AnthropicKey, cfg.AnthropicModel)
		log.Info("LLM: Anthropic", "model", cfg.AnthropicModel)
	} else {
		client = &llm.Mock{Delay: cfg.MockLLMDelay}
		log.Warn("LLM: mock (set ANTHROPIC_API_KEY to use Claude)")
	}

	reg := metrics.New()
	reg.Describe("composer_messages_sent_total", "Messages accepted by the provider.")
	reg.Describe("composer_messages_failed_total", "Messages that permanently failed.")
	reg.Describe("composer_messages_retried_total", "Retries scheduled (5xx, timeouts, 429).")
	reg.Describe("composer_provider_throttled_total", "429 responses received from the provider.")
	reg.Describe("composer_messages_skipped_total", "Messages skipped (e.g. consent revoked).")
	reg.Describe("composer_queue_messages", "Messages in the queue by channel and status.")
	reg.Gauge("composer_queue_messages", func() []metrics.Sample {
		qctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		depth, err := st.QueueDepth(qctx)
		if err != nil {
			return nil
		}
		var out []metrics.Sample
		for k, v := range depth {
			out = append(out, metrics.Sample{Labels: fmt.Sprintf(`channel=%q,status=%q`, k[0], k[1]), Value: float64(v)})
		}
		return out
	})

	engCfg := sender.DefaultConfig()
	engCfg.Workers, engCfg.MaxAttempts = cfg.Workers, cfg.MaxAttempts
	engine := &sender.Engine{
		Store:    st,
		Provider: provider.NewHTTP(cfg.ProviderURL),
		Limiters: map[string]sender.Limiter{"sms": ratelimit.New(cfg.RatePerSec), "email": ratelimit.New(cfg.RatePerSec)},
		Cfg:      engCfg, Metrics: reg, Log: log,
	}
	done := make(chan struct{})
	go func() { engine.Run(ctx); close(done) }()

	h := &httpapi.Handler{Store: st, LLM: &llm.Service{Client: client}, Metrics: reg, Log: log}
	srv := &http.Server{Addr: cfg.Addr, Handler: h.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	<-done
}

// connect retries for a while so the API tolerates Postgres starting slowly.
func connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, lastErr
}
