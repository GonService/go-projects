// Package upstream keeps the targets of a route, checks their health and
// picks the next healthy one in round-robin order.
package upstream

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Target is one upstream server.
type Target struct {
	URL *url.URL

	healthy  atomic.Bool
	requests atomic.Int64
	failures atomic.Int64 // proxy errors (connection refused, timeouts)

	mu        sync.Mutex
	okStreak  int
	badStreak int
	lastCheck time.Time
	latency   time.Duration
	lastError string
}

// Healthy reports whether the target receives traffic.
func (t *Target) Healthy() bool { return t.healthy.Load() }

// Snapshot is a read-only view of a target for status pages and metrics.
type Snapshot struct {
	URL       string        `json:"url"`
	Healthy   bool          `json:"healthy"`
	Requests  int64         `json:"requests"`
	Failures  int64         `json:"failures"`
	LastCheck time.Time     `json:"last_check"`
	Latency   time.Duration `json:"latency_ns"`
	LastError string        `json:"last_error,omitempty"`
}

// Snapshot returns the current state.
func (t *Target) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Snapshot{URL: t.URL.String(), Healthy: t.Healthy(), Requests: t.requests.Load(), Failures: t.failures.Load(),
		LastCheck: t.lastCheck, Latency: t.latency, LastError: t.lastError}
}

// CheckConfig controls active health checks.
type CheckConfig struct {
	Path           string
	Interval       time.Duration
	Timeout        time.Duration
	UnhealthyAfter int
	HealthyAfter   int
}

// Pool is the set of targets for one route.
type Pool struct {
	Targets []*Target
	check   CheckConfig
	client  *http.Client
	next    atomic.Uint64
}

// NewPool creates a pool. Targets start healthy so traffic flows before the
// first check completes; a failing target is removed within one interval.
func NewPool(urls []*url.URL, cc CheckConfig) *Pool {
	p := &Pool{check: cc, client: &http.Client{
		Timeout:       cc.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	for _, u := range urls {
		t := &Target{URL: u}
		t.healthy.Store(true)
		p.Targets = append(p.Targets, t)
	}
	return p
}

// Next returns the next healthy target in round-robin order, or nil when
// none is healthy.
func (p *Pool) Next() *Target {
	n := uint64(len(p.Targets))
	start := p.next.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		t := p.Targets[(start+i)%n]
		if t.Healthy() {
			t.requests.Add(1)
			return t
		}
	}
	return nil
}

// Healthy returns the number of healthy targets.
func (p *Pool) Healthy() int {
	n := 0
	for _, t := range p.Targets {
		if t.Healthy() {
			n++
		}
	}
	return n
}

// ReportFailure records a failed proxied request. It counts towards the
// unhealthy threshold, so a dead target is taken out before the next check.
func (p *Pool) ReportFailure(t *Target, err error) {
	t.failures.Add(1)
	p.record(t, false, false, 0, err.Error())
}

// Run checks every target each interval until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) {
	p.CheckAll(ctx)
	tick := time.NewTicker(p.check.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.CheckAll(ctx)
		}
	}
}

// CheckAll checks all targets in parallel and waits for the results.
func (p *Pool) CheckAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range p.Targets {
		wg.Add(1)
		go func(t *Target) {
			defer wg.Done()
			p.checkOne(ctx, t)
		}(t)
	}
	wg.Wait()
}

func (p *Pool) checkOne(ctx context.Context, t *Target) {
	u := *t.URL
	u.Path = strings.TrimRight(u.Path, "/") + p.check.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		p.record(t, false, true, 0, err.Error())
		return
	}
	req.Header.Set("User-Agent", "api-gateway-healthcheck")
	start := time.Now()
	res, err := p.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		p.record(t, false, true, elapsed, shortError(err))
		return
	}
	res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 400 {
		p.record(t, true, true, elapsed, "")
		return
	}
	p.record(t, false, true, elapsed, "health check returned "+res.Status)
}

// record updates the streaks; fromCheck is false for passive failures seen
// while proxying.
func (p *Pool) record(t *Target, ok, fromCheck bool, latency time.Duration, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fromCheck {
		t.lastCheck, t.latency = time.Now(), latency
	}
	if ok {
		t.okStreak++
		t.badStreak = 0
		t.lastError = ""
		if t.okStreak >= p.check.HealthyAfter {
			t.healthy.Store(true)
		}
		return
	}
	t.badStreak++
	t.okStreak = 0
	t.lastError = errMsg
	if t.badStreak >= p.check.UnhealthyAfter {
		t.healthy.Store(false)
	}
}

func shortError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "connection refused"
	case strings.Contains(s, "Client.Timeout") || strings.Contains(s, "deadline exceeded"):
		return "timeout"
	}
	return s
}
