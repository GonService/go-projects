// Package ratelimit implements per-client token buckets.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter holds one token bucket per key (API key name or client IP).
// Buckets that have been idle long enough to be full again are dropped, so
// memory stays bounded by the number of recently active clients.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter that allows burst requests at once and rps on average.
func New(rps float64, burst int) *Limiter {
	return &Limiter{rate: rps, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
}

// Result of a call to Allow.
type Result struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration // time until the next token when not allowed
}

// Allow takes one token from key's bucket.
func (l *Limiter) Allow(key string) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gc(now)

	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now

	res := Result{Limit: int(l.burst)}
	if b.tokens >= 1 {
		b.tokens--
		res.Allowed = true
		res.Remaining = int(b.tokens)
		return res
	}
	res.RetryAfter = time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return res
}

// Len returns the number of tracked clients.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// gc runs at most once a minute and removes buckets that are full again.
func (l *Limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
}
