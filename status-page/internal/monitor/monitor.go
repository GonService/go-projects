// Package monitor runs periodic HTTP checks and keeps a short history per target.
package monitor

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Target is one URL to check.
type Target struct {
	Name           string        `json:"name"`
	URL            string        `json:"url"`
	Interval       time.Duration `json:"-"`
	Timeout        time.Duration `json:"-"`
	ExpectedStatus int           `json:"-"`
}

// Result is the outcome of one check.
type Result struct {
	At      time.Time     `json:"at"`
	Up      bool          `json:"up"`
	Status  int           `json:"status,omitempty"`
	Latency time.Duration `json:"latency_ns"`
	Error   string        `json:"error,omitempty"`
}

// State is the public view of a target.
type State struct {
	Target      Target   `json:"target"`
	Up          bool     `json:"up"`
	Checked     bool     `json:"checked"`
	Last        *Result  `json:"last,omitempty"`
	Uptime      float64  `json:"uptime"` // 0..1 over the kept history
	AvgLatency  float64  `json:"avg_latency_ms"`
	History     []Result `json:"history"`
	Capacity    int      `json:"history_capacity"` // how many checks the history keeps
	TotalChecks uint64   `json:"total_checks"`
	TotalFails  uint64   `json:"total_failures"`
}

type entry struct {
	target  Target
	history []Result // oldest first, at most keep
	checks  uint64
	fails   uint64
}

// Monitor checks targets on their own interval. Safe for concurrent use.
type Monitor struct {
	mu      sync.RWMutex
	entries map[string]*entry
	order   []string
	keep    int
	client  *http.Client
	now     func() time.Time
}

// New creates a monitor that keeps the last `keep` results per target.
func New(targets []Target, keep int) *Monitor {
	m := &Monitor{
		entries: make(map[string]*entry, len(targets)),
		keep:    keep,
		client: &http.Client{
			// Do not follow redirects blindly: a 301 to a login page should not count as "up" unless expected.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("stopped after 5 redirects")
				}
				return nil
			},
		},
		now: time.Now,
	}
	for _, t := range targets {
		m.entries[t.Name] = &entry{target: t}
		m.order = append(m.order, t.Name)
	}
	return m
}

// Run starts one goroutine per target and blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range m.order {
		e := m.entries[name]
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			m.Check(ctx, t.Name)
			ticker := time.NewTicker(t.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.Check(ctx, t.Name)
				}
			}
		}(e.target)
	}
	wg.Wait()
}

// Check runs one check for the named target and records it.
func (m *Monitor) Check(ctx context.Context, name string) Result {
	m.mu.RLock()
	e, ok := m.entries[name]
	m.mu.RUnlock()
	if !ok {
		return Result{Error: "unknown target"}
	}
	t := e.target
	res := m.probe(ctx, t)

	m.mu.Lock()
	defer m.mu.Unlock()
	e.history = append(e.history, res)
	if len(e.history) > m.keep {
		e.history = e.history[len(e.history)-m.keep:]
	}
	e.checks++
	if !res.Up {
		e.fails++
	}
	return res
}

func (m *Monitor) probe(ctx context.Context, t Target) Result {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	start := m.now()
	res := Result{At: start}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("User-Agent", "gonservice-status-page/1.0")
	resp, err := m.client.Do(req)
	res.Latency = m.now().Sub(start)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	if t.ExpectedStatus != 0 {
		res.Up = resp.StatusCode == t.ExpectedStatus
	} else {
		res.Up = resp.StatusCode >= 200 && resp.StatusCode < 400
	}
	if !res.Up {
		res.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
	return res
}

// States returns a snapshot of every target, in configuration order.
func (m *Monitor) States() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]State, 0, len(m.order))
	for _, name := range m.order {
		e := m.entries[name]
		s := State{Target: e.target, TotalChecks: e.checks, TotalFails: e.fails, Capacity: m.keep}
		s.History = append([]Result(nil), e.history...)
		if n := len(e.history); n > 0 {
			last := e.history[n-1]
			s.Last, s.Up, s.Checked = &last, last.Up, true
			var up int
			var total time.Duration
			for _, r := range e.history {
				if r.Up {
					up++
				}
				total += r.Latency
			}
			s.Uptime = float64(up) / float64(n)
			s.AvgLatency = float64(total.Microseconds()) / float64(n) / 1000
		}
		out = append(out, s)
	}
	return out
}

// Overall summarizes all targets: "operational", "degraded" (some down) or "down" (all down).
func Overall(states []State) string {
	down, checked := 0, 0
	for _, s := range states {
		if !s.Checked {
			continue
		}
		checked++
		if !s.Up {
			down++
		}
	}
	switch {
	case checked == 0:
		return "pending"
	case down == 0:
		return "operational"
	case down == checked:
		return "down"
	default:
		return "degraded"
	}
}
