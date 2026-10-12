package ratelimit

import (
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newTest(rps float64, burst int) (*Limiter, *clock) {
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := New(rps, burst)
	l.now = c.now
	l.lastGC = c.t
	return l, c
}

func TestBurstThenRefill(t *testing.T) {
	l, c := newTest(2, 3) // 3 at once, then 2 per second
	for i := 0; i < 3; i++ {
		r := l.Allow("a")
		if !r.Allowed || r.Remaining != 2-i || r.Limit != 3 {
			t.Fatalf("request %d: %+v", i, r)
		}
	}
	r := l.Allow("a")
	if r.Allowed || r.RetryAfter != 500*time.Millisecond {
		t.Fatalf("over limit: %+v", r)
	}
	c.advance(250 * time.Millisecond)
	if l.Allow("a").Allowed {
		t.Fatal("half a token should not be enough")
	}
	c.advance(250 * time.Millisecond)
	if !l.Allow("a").Allowed {
		t.Fatal("token not refilled after 500ms")
	}
	c.advance(time.Hour)
	if r := l.Allow("a"); !r.Allowed || r.Remaining != 2 {
		t.Fatalf("bucket should be capped at burst: %+v", r)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTest(1, 1)
	if !l.Allow("a").Allowed || l.Allow("a").Allowed {
		t.Fatal("a")
	}
	if !l.Allow("b").Allowed {
		t.Fatal("b was limited by a's usage")
	}
}

func TestIdleBucketsAreDropped(t *testing.T) {
	l, c := newTest(1, 10)
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	c.advance(5 * time.Second)
	l.Allow("d")
	if l.Len() != 4 {
		t.Fatalf("len = %d before gc interval", l.Len())
	}
	c.advance(2 * time.Minute)
	l.Allow("e")
	if l.Len() != 1 {
		t.Fatalf("len = %d, idle buckets not dropped", l.Len())
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := New(0.001, 100) // effectively no refill during the test
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if l.Allow("shared").Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if allowed != 100 {
		t.Fatalf("allowed = %d, want exactly the burst (100)", allowed)
	}
}
