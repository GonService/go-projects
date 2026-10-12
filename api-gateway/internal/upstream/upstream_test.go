package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func server(t *testing.T, healthy *atomic.Bool) *url.URL {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/base/healthz" && !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL + "/base/")
	return u
}

var cc = CheckConfig{Path: "/healthz", Interval: time.Hour, Timeout: time.Second, UnhealthyAfter: 2, HealthyAfter: 1}

func TestRoundRobin(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	p := NewPool([]*url.URL{server(t, &up), server(t, &up), server(t, &up)}, cc)
	seen := map[*Target]int{}
	for i := 0; i < 9; i++ {
		seen[p.Next()]++
	}
	for _, tg := range p.Targets {
		if seen[tg] != 3 || tg.Snapshot().Requests != 3 {
			t.Fatalf("uneven distribution: %v", seen)
		}
	}
}

func TestHealthChecks(t *testing.T) {
	var a, b atomic.Bool
	a.Store(true)
	b.Store(true)
	p := NewPool([]*url.URL{server(t, &a), server(t, &b)}, cc)
	ctx := context.Background()

	p.CheckAll(ctx)
	if p.Healthy() != 2 || p.Targets[0].Snapshot().LastCheck.IsZero() {
		t.Fatal("both should be healthy after a check")
	}

	b.Store(false)
	p.CheckAll(ctx)
	if p.Healthy() != 2 {
		t.Fatal("one failure must not remove a target (unhealthy_after = 2)")
	}
	p.CheckAll(ctx)
	if p.Healthy() != 1 || p.Targets[1].Healthy() {
		t.Fatal("target b should be down after two failures")
	}
	if s := p.Targets[1].Snapshot(); s.LastError != "health check returned 503 Service Unavailable" {
		t.Fatalf("last error = %q", s.LastError)
	}
	for i := 0; i < 4; i++ {
		if p.Next() != p.Targets[0] {
			t.Fatal("traffic sent to an unhealthy target")
		}
	}

	a.Store(false)
	p.CheckAll(ctx)
	p.CheckAll(ctx)
	if p.Next() != nil {
		t.Fatal("Next should return nil when every target is down")
	}

	b.Store(true)
	p.CheckAll(ctx)
	if got := p.Next(); got != p.Targets[1] {
		t.Fatal("recovered target should get traffic again")
	}
}

func TestPassiveFailuresAndRefused(t *testing.T) {
	dead, _ := url.Parse("http://127.0.0.1:1")
	p := NewPool([]*url.URL{dead}, cc)
	tg := p.Targets[0]
	p.ReportFailure(tg, errors.New("dial tcp: connection refused"))
	if !tg.Healthy() {
		t.Fatal("one proxy error should not remove the target")
	}
	p.ReportFailure(tg, errors.New("dial tcp: connection refused"))
	if tg.Healthy() || tg.Snapshot().Failures != 2 {
		t.Fatalf("after two failures: %+v", tg.Snapshot())
	}
	p.CheckAll(context.Background())
	if tg.Snapshot().LastError != "connection refused" {
		t.Fatalf("last error = %q", tg.Snapshot().LastError)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	p := NewPool([]*url.URL{server(t, &up)}, CheckConfig{Path: "/healthz", Interval: 10 * time.Millisecond, Timeout: time.Second, UnhealthyAfter: 1, HealthyAfter: 1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}
