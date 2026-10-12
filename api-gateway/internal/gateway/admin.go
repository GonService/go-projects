package gateway

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/GonService/go-projects/api-gateway/internal/metrics"
	"github.com/GonService/go-projects/api-gateway/internal/upstream"
)

//go:embed templates/status.html
var files embed.FS

var statusTpl = template.Must(template.New("status.html").Funcs(template.FuncMap{
	"ms": func(d time.Duration) string {
		if d < time.Millisecond {
			return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000)
		}
		return fmt.Sprintf("%d ms", d.Milliseconds())
	},
	"clock": func(t time.Time) string { return t.Format("15:04:05") },
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return fmt.Sprintf("%ds ago", int(time.Since(t).Seconds()))
	},
	"short": func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	},
	"class": func(code int) string { return fmt.Sprintf("s%d", code/100) },
	"num":   func(f float64) string { return thousands(int64(f)) },
	"pct":   func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
	"since": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
}).ParseFS(files, "templates/status.html"))

// RouteStatus is the admin view of a route.
type RouteStatus struct {
	Name        string              `json:"name"`
	Prefix      string              `json:"prefix"`
	StripPrefix bool                `json:"strip_prefix"`
	Public      bool                `json:"public"`
	RPS         float64             `json:"rps"`
	Burst       int                 `json:"burst"`
	Healthy     int                 `json:"healthy"`
	Requests    float64             `json:"requests"`
	Targets     []upstream.Snapshot `json:"targets"`
}

// Status is the admin view of the gateway.
type Status struct {
	Started     time.Time     `json:"started"`
	Listen      string        `json:"listen"`
	Requests    float64       `json:"requests"`
	Errors5xx   float64       `json:"errors_5xx"`
	RateLimited float64       `json:"rate_limited"`
	AuthFailed  float64       `json:"auth_failures"`
	Clients     int           `json:"active_clients"`
	Healthy     int           `json:"healthy_upstreams"`
	Upstreams   int           `json:"upstreams"`
	Routes      []RouteStatus `json:"routes"`
	Recent      []Entry       `json:"recent"`
}

// ErrorRate is the share of 5xx responses in percent.
func (s Status) ErrorRate() float64 {
	if s.Requests == 0 {
		return 0
	}
	return s.Errors5xx * 100 / s.Requests
}

// Status collects the current state.
func (g *Gateway) Status() Status {
	s := Status{
		Started:     g.started,
		Listen:      g.cfg.Listen,
		Requests:    g.requests.Sum(""),
		Errors5xx:   g.requests.Sum(`code="5`),
		RateLimited: g.rateLimited.Sum(""),
		AuthFailed:  g.authFailed.Sum(""),
		Clients:     g.limiter.Len(),
		Recent:      g.Recent(),
	}
	for _, r := range g.ordered {
		rs := RouteStatus{Name: r.Config.Name, Prefix: r.Config.Prefix, StripPrefix: r.Config.StripPrefix, Public: r.Config.Public,
			RPS: g.cfg.RateLimit.RPS, Burst: g.cfg.RateLimit.Burst, Healthy: r.Pool.Healthy(),
			Requests: g.requests.Sum(metrics.Labels("route", r.Config.Name) + ",")}
		if rl := r.Config.RateLimit; rl != nil {
			rs.RPS, rs.Burst = rl.RPS, rl.Burst
			s.Clients += r.Limiter.Len()
		}
		for _, t := range r.Pool.Targets {
			rs.Targets = append(rs.Targets, t.Snapshot())
		}
		s.Healthy += rs.Healthy
		s.Upstreams += len(rs.Targets)
		s.Routes = append(s.Routes, rs)
	}
	return s
}

// AdminHandler serves the status page, JSON status, metrics and health on
// the admin listener. Keep that listener on a private network.
func (g *Gateway) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = statusTpl.Execute(w, g.Status())
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(g.Status())
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		g.writeMetrics(w)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
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

func (g *Gateway) writeMetrics(w http.ResponseWriter) {
	g.requests.Write(w)
	g.duration.Write(w)
	g.rateLimited.Write(w)
	g.authFailed.Write(w)
	g.upstreamErr.Write(w)
	up := map[string]float64{}
	for _, r := range g.ordered {
		for _, t := range r.Pool.Targets {
			v := 0.0
			if t.Healthy() {
				v = 1
			}
			up[metrics.Labels("route", r.Config.Name, "target", t.URL.String())] = v
		}
	}
	metrics.Gauge(w, "gateway_upstream_up", "Whether the upstream target is healthy (1) and receives traffic.", up)
}

func thousands(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
