// Package ratelimit implements a strict pacer: requests are spaced at least
// 1/rate apart, so no one-second window can ever contain more than `rate`
// requests (a token bucket with a burst would allow bursts above the limit).
package ratelimit

import (
	"context"
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type Pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	clock    Clock
}

func New(ratePerSec int) *Pacer { return NewWithClock(ratePerSec, realClock{}) }

func NewWithClock(ratePerSec int, c Clock) *Pacer {
	if ratePerSec < 1 {
		ratePerSec = 1
	}
	return &Pacer{interval: time.Second / time.Duration(ratePerSec), clock: c}
}

// Wait reserves the next send slot and blocks until it arrives.
func (p *Pacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	now := p.clock.Now()
	if p.next.Before(now) {
		p.next = now
	}
	slot := p.next
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()
	return p.clock.Sleep(ctx, slot.Sub(now))
}

// Pause pushes every future slot back by at least d (used after a 429).
func (p *Pacer) Pause(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until := p.clock.Now().Add(d)
	if until.After(p.next) {
		p.next = until
	}
}
