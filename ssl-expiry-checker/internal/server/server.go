// Package server runs checks on a schedule and serves the latest results as
// Prometheus metrics, JSON and an HTML page (daemon mode).
package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/GonService/go-projects/ssl-expiry-checker/internal/check"
	"github.com/GonService/go-projects/ssl-expiry-checker/internal/report"
)

// Server keeps the results of the last run.
type Server struct {
	targets []check.Target
	opt     check.Options

	mu      sync.RWMutex
	results []check.Result
	at      time.Time
}

// New returns a Server; call Refresh or Run to populate it.
func New(targets []check.Target, opt check.Options) *Server {
	return &Server{targets: targets, opt: opt}
}

// Refresh checks every target once.
func (s *Server) Refresh(ctx context.Context) {
	res := check.Run(ctx, s.targets, s.opt)
	s.mu.Lock()
	s.results, s.at = res, time.Now()
	s.mu.Unlock()
}

// Run refreshes immediately and then every interval until ctx is done.
func (s *Server) Run(ctx context.Context, every time.Duration) {
	s.Refresh(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Refresh(ctx)
		}
	}
}

func (s *Server) snapshot() ([]check.Result, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.results, s.at
}

// Handler serves /, /api/results, /metrics and /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	meta := func(at time.Time) report.Meta {
		return report.Meta{Generated: at, WarnDays: s.opt.WarnDays, CritDays: s.opt.CritDays, Refresh: 60}
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		res, at := s.snapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = report.HTML(w, res, meta(at))
	})
	mux.HandleFunc("GET /api/results", func(w http.ResponseWriter, r *http.Request) {
		res, at := s.snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = report.JSON(w, res, meta(at))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		res, _ := s.snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		report.Metrics(w, res)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, at := s.snapshot(); at.IsZero() {
			http.Error(w, "first check still running", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		mux.ServeHTTP(w, r)
	})
}
