// mockprovider imitates a real SMS/email gateway closely enough to exercise
// retry, backoff, rate limiting and idempotency:
//
//   - random latency
//   - random 500s                      (PROVIDER_ERROR_RATE)
//   - random 429 with Retry-After      (PROVIDER_429_RATE)
//   - a hard per-channel limit that answers 429 when exceeded (PROVIDER_LIMIT_PER_SEC)
//   - "lost response": the message IS accepted but the reply is delayed past the
//     client's timeout, so the client retries (PROVIDER_TIMEOUT_RATE)
//   - Idempotency-Key support: a replay returns the original result and is not
//     delivered again.
//
// GET /stats reports what was really "delivered", which the demo and the
// README use to show nothing was delivered twice.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type result struct {
	ID      string
	Channel string
}

type server struct {
	mu          sync.Mutex
	byKey       map[string]result
	deliveries  map[string]int // idempotency key -> times delivered
	chanOf      map[string]string
	received    map[string]int
	replays     int
	injected500 int
	injected429 int
	limited429  int
	lostReplies int
	windows     map[string]*window
	seq         int

	errRate, rate429, timeoutRate float64
	limitPerSec                   int
	timeoutHold                   time.Duration
	minLat, maxLat                time.Duration
}

type window struct {
	start time.Time
	n     int
}

func envF(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func main() {
	s := &server{
		byKey: map[string]result{}, deliveries: map[string]int{}, chanOf: map[string]string{}, received: map[string]int{}, windows: map[string]*window{},
		errRate: envF("PROVIDER_ERROR_RATE", 0.05), rate429: envF("PROVIDER_429_RATE", 0.03), timeoutRate: envF("PROVIDER_TIMEOUT_RATE", 0.005),
		limitPerSec: int(envF("PROVIDER_LIMIT_PER_SEC", 150)),
		timeoutHold: time.Duration(envF("PROVIDER_TIMEOUT_HOLD_MS", 6000)) * time.Millisecond,
		minLat:      time.Duration(envF("PROVIDER_MIN_LATENCY_MS", 20)) * time.Millisecond,
		maxLat:      time.Duration(envF("PROVIDER_MAX_LATENCY_MS", 150)) * time.Millisecond,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sms", s.handle("sms"))
	mux.HandleFunc("POST /v1/email", s.handle("email"))
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("POST /reset", s.reset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	addr := ":" + envOr("PORT", "9090")
	log.Printf("mock provider on %s (err=%.2f 429=%.2f timeout=%.3f limit=%d/s)", addr, s.errRate, s.rate429, s.timeoutRate, s.limitPerSec)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (s *server) handle(channel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.To == "" || req.Body == "" {
			http.Error(w, `{"error":"to and body are required"}`, http.StatusBadRequest)
			return
		}
		if channel == "sms" && len([]rune(req.Body)) > 160 {
			http.Error(w, `{"error":"sms body exceeds 160 characters"}`, http.StatusBadRequest)
			return
		}
		if channel == "email" && (req.Subject == "" || len([]rune(req.Subject)) > 80) {
			http.Error(w, `{"error":"email subject must be 1-80 characters"}`, http.StatusBadRequest)
			return
		}
		key := r.Header.Get("Idempotency-Key")

		s.mu.Lock()
		s.received[channel]++
		// replay of a request we already accepted: return the original answer
		if key != "" {
			if prev, ok := s.byKey[key]; ok {
				s.replays++
				s.mu.Unlock()
				w.Header().Set("X-Idempotent-Replay", "true")
				writeJSON(w, http.StatusOK, map[string]string{"id": prev.ID})
				return
			}
		}
		// hard provider-side limit
		if s.limitPerSec > 0 {
			win := s.windows[channel]
			now := time.Now()
			if win == nil || now.Sub(win.start) >= time.Second {
				win = &window{start: now}
				s.windows[channel] = win
			}
			win.n++
			if win.n > s.limitPerSec {
				s.limited429++
				s.mu.Unlock()
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
		}
		roll := rand.Float64()
		s.mu.Unlock()

		time.Sleep(s.minLat + time.Duration(rand.Int63n(int64(s.maxLat-s.minLat)+1)))

		switch {
		case roll < s.errRate:
			s.mu.Lock()
			s.injected500++
			s.mu.Unlock()
			http.Error(w, `{"error":"upstream unavailable"}`, http.StatusServiceUnavailable)
			return
		case roll < s.errRate+s.rate429:
			s.mu.Lock()
			s.injected429++
			s.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
			return
		}

		// accept + deliver
		s.mu.Lock()
		s.seq++
		res := result{ID: fmt.Sprintf("%s_%08d", channel, s.seq), Channel: channel}
		dkey := key
		if dkey == "" {
			dkey = "nokey:" + res.ID
		}
		s.deliveries[dkey]++
		s.chanOf[dkey] = channel
		if key != "" {
			s.byKey[key] = res
		}
		lost := rand.Float64() < s.timeoutRate
		if lost {
			s.lostReplies++
		}
		s.mu.Unlock()

		if lost { // delivered, but the client will time out waiting for the answer
			time.Sleep(s.timeoutHold)
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": res.ID})
	}
}

func (s *server) stats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	perChannel := map[string]int{}
	maxPerKey := 0
	for k, n := range s.deliveries {
		perChannel[s.chanOf[k]]++
		if n > maxPerKey {
			maxPerKey = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests_received":           s.received,
		"unique_deliveries":           len(s.deliveries),
		"deliveries_by_channel":       perChannel,
		"max_deliveries_for_one_key":  maxPerKey, // must be 1
		"idempotent_replays":          s.replays,
		"injected_5xx":                s.injected500,
		"injected_429":                s.injected429,
		"rate_limit_429":              s.limited429,
		"lost_replies_after_delivery": s.lostReplies,
	})
}

func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.byKey, s.deliveries, s.chanOf, s.received = map[string]result{}, map[string]int{}, map[string]string{}, map[string]int{}
	s.replays, s.injected500, s.injected429, s.limited429, s.lostReplies = 0, 0, 0, 0, 0
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
