// Package sender is the delivery engine: a durable queue (rows in Postgres)
// drained by per-channel worker pools behind a strict rate limiter.
//
// Delivery semantics: at-least-once from the queue's point of view, made
// effectively exactly-once by sending the same Idempotency-Key on every attempt
// of a message and by the UNIQUE(campaign, user, channel) constraint that stops
// a message from being enqueued twice.
package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"sync"
	"time"

	"campaigncomposer/internal/metrics"
)

var Channels = []string{"sms", "email"}

type Message struct {
	ID             int64
	CampaignID     string
	UserID         int64
	Channel        string
	Recipient      string
	Subject        string
	Body           string
	IdempotencyKey string
	Attempts       int
	Throttled      int
	ConsentOK      bool // channel opt-in as of claim time
}

// Store is the persistence the engine needs. The Postgres implementation lives
// in internal/store; tests use an in-memory fake.
type Store interface {
	ClaimBatch(ctx context.Context, channel string, limit int, staleAfter time.Duration) ([]Message, error)
	MarkSent(ctx context.Context, id int64, providerID string) error
	MarkRetry(ctx context.Context, id int64, next time.Time, lastErr string, countAttempt bool) error
	MarkFailed(ctx context.Context, id int64, lastErr string) error
	MarkSkipped(ctx context.Context, id int64, reason string) error
	CompleteFinishedCampaigns(ctx context.Context) error
}

type Provider interface {
	Send(ctx context.Context, m Message) (providerID string, err error)
}

// Limiter is satisfied by *ratelimit.Pacer.
type Limiter interface {
	Wait(ctx context.Context) error
	Pause(d time.Duration)
}

// ProviderError carries the HTTP status so the engine can classify failures.
type ProviderError struct {
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider status %d: %v", e.Status, e.Err)
}
func (e *ProviderError) Unwrap() error { return e.Err }

type Config struct {
	Workers      int
	Batch        int
	MaxAttempts  int // for 5xx / network failures
	MaxThrottles int // for 429s
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	PollEvery    time.Duration
	StaleAfter   time.Duration
	SendTimeout  time.Duration
	// MaxChannelPause caps how long a single 429 stalls the whole channel. The
	// message itself always waits the full Retry-After; the channel only backs
	// off briefly so sporadic 429s do not starve throughput.
	MaxChannelPause time.Duration
}

func DefaultConfig() Config {
	return Config{Workers: 32, Batch: 64, MaxAttempts: 5, MaxThrottles: 50, BaseBackoff: 2 * time.Second, MaxBackoff: 2 * time.Minute,
		PollEvery: 500 * time.Millisecond, StaleAfter: 60 * time.Second, SendTimeout: 5 * time.Second, MaxChannelPause: 500 * time.Millisecond}
}

type Engine struct {
	Store    Store
	Provider Provider
	Limiters map[string]Limiter
	Cfg      Config
	Metrics  *metrics.Registry
	Log      *slog.Logger
	Now      func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Engine) inc(name, labels string) {
	if e.Metrics != nil {
		e.Metrics.Inc(name, labels)
	}
}

// Run blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, ch := range Channels {
		ch := ch
		jobs := make(chan Message)
		wg.Add(1)
		go func() { defer wg.Done(); e.produce(ctx, ch, jobs) }()
		for i := 0; i < e.Cfg.Workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for m := range jobs {
					e.Process(ctx, m)
				}
			}()
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := e.Store.CompleteFinishedCampaigns(ctx); err != nil && ctx.Err() == nil {
					e.log().Error("complete campaigns", "err", err)
				}
			}
		}
	}()
	wg.Wait()
}

func (e *Engine) produce(ctx context.Context, ch string, jobs chan<- Message) {
	defer close(jobs)
	for ctx.Err() == nil {
		msgs, err := e.Store.ClaimBatch(ctx, ch, e.Cfg.Batch, e.Cfg.StaleAfter)
		if err != nil {
			if ctx.Err() == nil {
				e.log().Error("claim batch", "channel", ch, "err", err)
			}
			sleep(ctx, e.Cfg.PollEvery*2)
			continue
		}
		if len(msgs) == 0 {
			sleep(ctx, e.Cfg.PollEvery)
			continue
		}
		for _, m := range msgs {
			select {
			case jobs <- m:
			case <-ctx.Done():
				return // claimed rows stay 'sending' and are re-queued by the stale reaper
			}
		}
	}
}

type outcome int

const (
	outSent outcome = iota
	outThrottled
	outRetry
	outPermanent
)

// Process delivers one claimed message and records the outcome.
func (e *Engine) Process(ctx context.Context, m Message) {
	lbl := fmt.Sprintf(`channel=%q`, m.Channel)
	// Results are written with a context that survives shutdown so a message
	// that was actually sent is never re-sent just because we were stopping.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	// Consent is checked again at the last moment (claim query joins users).
	if !m.ConsentOK {
		e.inc("composer_messages_skipped_total", lbl+`,reason="no_consent"`)
		if err := e.Store.MarkSkipped(wctx, m.ID, "no_consent"); err != nil {
			e.log().Error("mark skipped", "id", m.ID, "err", err)
		}
		return
	}
	if lim := e.Limiters[m.Channel]; lim != nil {
		if err := lim.Wait(ctx); err != nil {
			return // shutting down; row is recovered by the stale reaper
		}
	}
	sctx, scancel := context.WithTimeout(ctx, e.Cfg.SendTimeout)
	pid, err := e.Provider.Send(sctx, m)
	scancel()
	e.inc("composer_provider_requests_total", lbl)

	switch kind, retryAfter := classify(err); kind {
	case outSent:
		e.inc("composer_messages_sent_total", lbl)
		if err := e.Store.MarkSent(wctx, m.ID, pid); err != nil {
			e.log().Error("mark sent", "id", m.ID, "err", err)
		}
	case outThrottled:
		e.inc("composer_provider_throttled_total", lbl)
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		if lim := e.Limiters[m.Channel]; lim != nil {
			lim.Pause(min(retryAfter, e.Cfg.MaxChannelPause)) // brief channel-wide backoff, not just this message
		}
		if m.Throttled+1 >= e.Cfg.MaxThrottles {
			e.fail(wctx, m, lbl, "throttled too many times: "+err.Error())
			return
		}
		e.retry(wctx, m, lbl, e.now().Add(retryAfter), err.Error(), false)
	case outRetry:
		if m.Attempts+1 >= e.Cfg.MaxAttempts {
			e.fail(wctx, m, lbl, fmt.Sprintf("gave up after %d attempts: %v", m.Attempts+1, err))
			return
		}
		e.retry(wctx, m, lbl, e.now().Add(Backoff(m.Attempts+1, e.Cfg.BaseBackoff, e.Cfg.MaxBackoff, rand.Float64())), err.Error(), true)
	case outPermanent:
		e.fail(wctx, m, lbl, "rejected by provider: "+err.Error())
	}
}

func (e *Engine) retry(ctx context.Context, m Message, lbl string, next time.Time, msg string, countAttempt bool) {
	e.inc("composer_messages_retried_total", lbl)
	if err := e.Store.MarkRetry(ctx, m.ID, next, msg, countAttempt); err != nil {
		e.log().Error("mark retry", "id", m.ID, "err", err)
	}
}

func (e *Engine) fail(ctx context.Context, m Message, lbl, msg string) {
	e.inc("composer_messages_failed_total", lbl)
	if err := e.Store.MarkFailed(ctx, m.ID, msg); err != nil {
		e.log().Error("mark failed", "id", m.ID, "err", err)
	}
}

// classify maps a provider result to what the engine should do next.
func classify(err error) (outcome, time.Duration) {
	if err == nil {
		return outSent, 0
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		switch {
		case pe.Status == 429:
			return outThrottled, pe.RetryAfter
		case pe.Status == 408 || pe.Status >= 500:
			return outRetry, 0
		case pe.Status >= 400:
			return outPermanent, 0 // 4xx other than 408/429: retrying cannot help
		}
	}
	// timeouts, connection resets, DNS failures: the request may or may not have
	// been processed, which is exactly why every retry reuses the idempotency key.
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return outRetry, 0
	}
	return outRetry, 0
}

// Backoff is exponential with ±20% jitter: base*2^(attempt-1), capped.
// jitter01 is a uniform [0,1) sample (injected so it is testable).
func Backoff(attempt int, base, max time.Duration, jitter01 float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := float64(base) * math.Pow(2, float64(attempt-1))
	if d > float64(max) {
		d = float64(max)
	}
	factor := 0.8 + 0.4*jitter01
	return time.Duration(d * factor)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
