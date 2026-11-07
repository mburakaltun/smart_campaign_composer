package sender

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"campaigncomposer/internal/metrics"
)

// ---- in-memory fakes -------------------------------------------------------

type row struct {
	Message
	status string
	next   time.Time
	err    string
	pid    string
}

type fakeStore struct {
	mu   sync.Mutex
	rows map[int64]*row
}

func newStore(n int, channel string) *fakeStore {
	s := &fakeStore{rows: map[int64]*row{}}
	for i := 1; i <= n; i++ {
		s.rows[int64(i)] = &row{Message: Message{ID: int64(i), UserID: int64(i), Channel: channel, Recipient: fmt.Sprintf("r%d", i),
			Body: "hi", IdempotencyKey: fmt.Sprintf("camp:%d:%s", i, channel), ConsentOK: true}, status: "pending"}
	}
	return s
}

func (s *fakeStore) ClaimBatch(_ context.Context, ch string, limit int, _ time.Duration) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for id := int64(1); id <= int64(len(s.rows)) && len(out) < limit; id++ {
		r := s.rows[id]
		if r.Channel == ch && r.status == "pending" && !r.next.After(time.Now()) {
			r.status = "sending"
			out = append(out, r.Message)
		}
	}
	return out, nil
}
func (s *fakeStore) set(id int64, f func(r *row)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.rows[id])
	return nil
}
func (s *fakeStore) MarkSent(_ context.Context, id int64, pid string) error {
	return s.set(id, func(r *row) { r.status, r.pid = "sent", pid })
}
func (s *fakeStore) MarkRetry(_ context.Context, id int64, next time.Time, e string, count bool) error {
	return s.set(id, func(r *row) {
		r.status, r.next, r.err = "pending", next, e
		if count {
			r.Attempts++
		} else {
			r.Throttled++
		}
	})
}
func (s *fakeStore) MarkFailed(_ context.Context, id int64, e string) error {
	return s.set(id, func(r *row) { r.status, r.err = "failed", e })
}
func (s *fakeStore) MarkSkipped(_ context.Context, id int64, e string) error {
	return s.set(id, func(r *row) { r.status, r.err = "skipped", e })
}
func (s *fakeStore) CompleteFinishedCampaigns(context.Context) error { return nil }
func (s *fakeStore) get(id int64) row {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.rows[id]
}
func (s *fakeStore) count(status string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		if r.status == status {
			n++
		}
	}
	return n
}

// scriptedProvider returns queued results per call and records calls.
type scriptedProvider struct {
	mu      sync.Mutex
	results []error
	calls   []Message
}

func (p *scriptedProvider) Send(_ context.Context, m Message) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, m)
	if len(p.results) > 0 {
		r := p.results[0]
		p.results = p.results[1:]
		if r != nil {
			return "", r
		}
	}
	return "prov-" + m.IdempotencyKey, nil
}

type spyLimiter struct {
	mu     sync.Mutex
	waits  int
	paused []time.Duration
}

func (l *spyLimiter) Wait(context.Context) error { l.mu.Lock(); l.waits++; l.mu.Unlock(); return nil }
func (l *spyLimiter) Pause(d time.Duration) {
	l.mu.Lock()
	l.paused = append(l.paused, d)
	l.mu.Unlock()
}

func engine(st Store, p Provider, lim Limiter) *Engine {
	cfg := DefaultConfig()
	cfg.BaseBackoff, cfg.MaxBackoff, cfg.PollEvery = time.Millisecond, 5*time.Millisecond, time.Millisecond
	return &Engine{Store: st, Provider: p, Limiters: map[string]Limiter{"sms": lim, "email": lim}, Cfg: cfg, Metrics: metrics.New()}
}

func provErr(status int, ra time.Duration) error {
	return &ProviderError{Status: status, RetryAfter: ra, Err: errors.New("boom")}
}

// ---- unit tests -------------------------------------------------------------

func TestProcessSuccess(t *testing.T) {
	st, p, lim := newStore(1, "sms"), &scriptedProvider{}, &spyLimiter{}
	engine(st, p, lim).Process(context.Background(), st.get(1).Message)
	if r := st.get(1); r.status != "sent" || r.pid == "" {
		t.Fatalf("row: %+v", r)
	}
	if lim.waits != 1 {
		t.Errorf("limiter waits=%d", lim.waits)
	}
}

func TestServerErrorIsRetriedWithBackoffAndSameKey(t *testing.T) {
	st, p := newStore(1, "sms"), &scriptedProvider{results: []error{provErr(503, 0)}}
	e := engine(st, p, &spyLimiter{})
	base := time.Unix(1_700_000_000, 0)
	e.Now = func() time.Time { return base }
	e.Process(context.Background(), st.get(1).Message)
	r := st.get(1)
	if r.status != "pending" || r.Attempts != 1 || !r.next.After(base) {
		t.Fatalf("expected scheduled retry, got %+v", r)
	}
	e.Process(context.Background(), r.Message)
	if st.get(1).status != "sent" {
		t.Fatal("second attempt should succeed")
	}
	if len(p.calls) != 2 || p.calls[0].IdempotencyKey != p.calls[1].IdempotencyKey {
		t.Fatal("retries must reuse the idempotency key")
	}
}

func TestRateLimitedPausesChannelAndDoesNotBurnAttempts(t *testing.T) {
	st, p, lim := newStore(1, "sms"), &scriptedProvider{results: []error{provErr(429, 3*time.Second)}}, &spyLimiter{}
	e := engine(st, p, lim)
	e.Process(context.Background(), st.get(1).Message)
	r := st.get(1)
	if r.status != "pending" || r.Attempts != 0 || r.Throttled != 1 {
		t.Fatalf("429 must not consume an attempt: %+v", r)
	}
	if len(lim.paused) != 1 || lim.paused[0] != e.Cfg.MaxChannelPause {
		t.Fatalf("channel should back off (capped), paused=%v", lim.paused)
	}
	if r.next.Before(time.Now().Add(2 * time.Second)) {
		t.Fatalf("the message itself must wait the full Retry-After, next=%v", r.next)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	st, p := newStore(1, "email"), &scriptedProvider{results: []error{provErr(400, 0)}}
	engine(st, p, &spyLimiter{}).Process(context.Background(), st.get(1).Message)
	if r := st.get(1); r.status != "failed" {
		t.Fatalf("400 should fail permanently: %+v", r)
	}
	if len(p.calls) != 1 {
		t.Fatal("must not retry a 4xx")
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	st, p := newStore(1, "sms"), &scriptedProvider{}
	for i := 0; i < 10; i++ {
		p.results = append(p.results, provErr(500, 0))
	}
	e := engine(st, p, &spyLimiter{})
	for i := 0; i < e.Cfg.MaxAttempts; i++ {
		e.Process(context.Background(), st.get(1).Message)
	}
	if r := st.get(1); r.status != "failed" || r.Attempts != e.Cfg.MaxAttempts-1 {
		t.Fatalf("want failed after %d tries: %+v", e.Cfg.MaxAttempts, r)
	}
	if len(p.calls) != e.Cfg.MaxAttempts {
		t.Fatalf("calls=%d", len(p.calls))
	}
}

func TestNoConsentIsSkippedAndNeverSent(t *testing.T) {
	st, p := newStore(1, "sms"), &scriptedProvider{}
	m := st.get(1).Message
	m.ConsentOK = false // user revoked consent after the campaign was approved
	engine(st, p, &spyLimiter{}).Process(context.Background(), m)
	if r := st.get(1); r.status != "skipped" || r.err != "no_consent" {
		t.Fatalf("row: %+v", r)
	}
	if len(p.calls) != 0 {
		t.Fatal("provider must not be called without consent")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	b := func(a int) time.Duration { return Backoff(a, time.Second, 10*time.Second, 0.5) }
	if !(b(1) < b(2) && b(2) < b(3)) {
		t.Fatal("backoff must grow")
	}
	if b(10) != 10*time.Second || b(30) != 10*time.Second {
		t.Fatalf("cap broken: %v %v", b(10), b(30))
	}
	if lo, hi := Backoff(2, time.Second, time.Minute, 0), Backoff(2, time.Second, time.Minute, 0.999); lo >= hi || lo < 1600*time.Millisecond || hi > 2400*time.Millisecond {
		t.Fatalf("jitter outside ±20%%: %v..%v", lo, hi)
	}
}

// ---- end-to-end through Run with a flaky provider ---------------------------

// flakyProvider simulates the nasty case: the provider processes the request
// but the response is lost (timeout), so the engine must retry — and dedupe by
// idempotency key keeps the recipient from receiving it twice.
type flakyProvider struct {
	mu        sync.Mutex
	delivered map[string]int // idempotency key -> times actually delivered
	calls     int
}

func (f *flakyProvider) Send(_ context.Context, m Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	_, seen := f.delivered[m.IdempotencyKey]
	switch f.calls % 7 {
	case 1:
		return "", provErr(500, 0) // before processing
	case 3:
		return "", provErr(429, time.Millisecond)
	case 5: // processed, but the answer never arrives
		if !seen {
			f.delivered[m.IdempotencyKey]++
		}
		return "", context.DeadlineExceeded
	}
	if !seen {
		f.delivered[m.IdempotencyKey]++
	}
	return "id-" + m.IdempotencyKey, nil
}

func TestRunDeliversEveryMessageExactlyOnceDespiteFailures(t *testing.T) {
	const n = 200
	st := newStore(n, "sms")
	fp := &flakyProvider{delivered: map[string]int{}}
	e := engine(st, fp, &spyLimiter{})
	e.Cfg.Workers, e.Cfg.Batch = 8, 16
	e.Cfg.MaxAttempts = 20 // this test is about exactly-once, not about giving up

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	for st.count("sent") < n && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := st.count("sent"); got != n {
		t.Fatalf("sent=%d want %d (failed=%d pending=%d)", got, n, st.count("failed"), st.count("pending"))
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for k, c := range fp.delivered {
		if c != 1 {
			t.Fatalf("key %s delivered %d times", k, c)
		}
	}
	if len(fp.delivered) != n {
		t.Fatalf("delivered %d unique keys, want %d", len(fp.delivered), n)
	}
	if fp.calls <= n {
		t.Fatal("test provider never failed; scenario not exercised")
	}
}
