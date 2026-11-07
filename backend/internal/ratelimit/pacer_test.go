package ratelimit

import (
	"context"
	"testing"
	"time"
)

// fakeClock advances virtual time instead of sleeping.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	if d > 0 {
		f.now = f.now.Add(d)
	}
	return nil
}

func TestNeverExceedsRatePerSecondWindow(t *testing.T) {
	fc := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	p := NewWithClock(100, fc)
	var sends []time.Time
	for i := 0; i < 1000; i++ {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		sends = append(sends, fc.now)
	}
	// every half-open 1s window must hold at most 100 sends
	for i := range sends {
		n := 0
		for j := i; j < len(sends) && sends[j].Sub(sends[i]) < time.Second; j++ {
			n++
		}
		if n > 100 {
			t.Fatalf("window starting at send %d holds %d sends (>100)", i, n)
		}
	}
	if total := sends[len(sends)-1].Sub(sends[0]); total < 9*time.Second {
		t.Fatalf("1000 sends at 100/s must take ~10s, took %v", total)
	}
}

func TestPauseDelaysFollowingSlots(t *testing.T) {
	fc := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	p := NewWithClock(100, fc)
	_ = p.Wait(context.Background())
	start := fc.now
	p.Pause(2 * time.Second)
	_ = p.Wait(context.Background())
	if fc.now.Sub(start) < 2*time.Second {
		t.Fatalf("pause not honoured: %v", fc.now.Sub(start))
	}
}

func TestWaitHonoursContextCancellation(t *testing.T) {
	p := New(1)
	_ = p.Wait(context.Background()) // first slot is immediate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); err == nil {
		t.Fatal("expected context error")
	}
}
