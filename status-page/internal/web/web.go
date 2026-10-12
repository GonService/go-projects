// Package web serves the status page, a JSON API and Prometheus metrics.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/GonService/go-projects/status-page/internal/monitor"
)

//go:embed templates/*.html
var files embed.FS

// Source provides the current state; *monitor.Monitor implements it.
type Source interface {
	States() []monitor.State
}

// Handler builds the HTTP routes.
func Handler(title string, src Source) http.Handler {
	tpl := template.Must(template.New("").Funcs(template.FuncMap{
		"pct":   func(f float64) string { return fmt.Sprintf("%.2f%%", f*100) },
		"ms":    func(f float64) string { return fmt.Sprintf("%.0f ms", f) },
		"ago":   ago,
		"lower": strings.ToLower,
		// empty returns placeholders so the history bar keeps a fixed width while it fills up
		"empty": func(capacity, used int) []struct{} {
			if capacity <= used {
				return nil
			}
			return make([]struct{}, capacity-used)
		},
	}).ParseFS(files, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		states := src.States()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = tpl.ExecuteTemplate(w, "index.html", map[string]any{
			"Title":    title,
			"Overall":  monitor.Overall(states),
			"Services": states,
			"Now":      time.Now(),
		})
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		states := src.States()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(map[string]any{"title": title, "status": monitor.Overall(states), "services": states})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, src.States())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return securityHeaders(mux)
}

func writeMetrics(w http.ResponseWriter, states []monitor.State) {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace
	fmt.Fprintln(w, "# HELP status_page_up Whether the last check succeeded (1) or failed (0).")
	fmt.Fprintln(w, "# TYPE status_page_up gauge")
	for _, s := range states {
		if s.Checked {
			v := 0
			if s.Up {
				v = 1
			}
			fmt.Fprintf(w, "status_page_up{service=\"%s\"} %d\n", esc(s.Target.Name), v)
		}
	}
	fmt.Fprintln(w, "# HELP status_page_latency_seconds Latency of the last check.")
	fmt.Fprintln(w, "# TYPE status_page_latency_seconds gauge")
	for _, s := range states {
		if s.Last != nil {
			fmt.Fprintf(w, "status_page_latency_seconds{service=\"%s\"} %.3f\n", esc(s.Target.Name), s.Last.Latency.Seconds())
		}
	}
	fmt.Fprintln(w, "# HELP status_page_checks_total Checks performed.")
	fmt.Fprintln(w, "# TYPE status_page_checks_total counter")
	for _, s := range states {
		fmt.Fprintf(w, "status_page_checks_total{service=\"%s\"} %d\n", esc(s.Target.Name), s.TotalChecks)
	}
	fmt.Fprintln(w, "# HELP status_page_failures_total Failed checks.")
	fmt.Fprintln(w, "# TYPE status_page_failures_total counter")
	for _, s := range states {
		fmt.Fprintf(w, "status_page_failures_total{service=\"%s\"} %d\n", esc(s.Target.Name), s.TotalFails)
	}
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}
