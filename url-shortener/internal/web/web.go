// Package web serves the HTML form, the JSON API, redirects and metrics.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GonService/go-projects/url-shortener/internal/store"
)

//go:embed templates/*.html
var files embed.FS

// Server holds the handler state.
type Server struct {
	store   *store.Store
	baseURL string // optional; derived from the request when empty
	tpl     *template.Template

	created   atomic.Int64
	redirects atomic.Int64
	notFound  atomic.Int64
}

// New returns a Server. baseURL (for example "https://go.example.com") is
// used to build short links; leave it empty to use the request host.
func New(s *store.Store, baseURL string) *Server {
	tpl := template.Must(template.New("").Funcs(template.FuncMap{
		"date": func(t time.Time) string { return t.Local().Format("02/01/2006 15:04") },
		"thousands": func(n int64) string {
			s := fmt.Sprint(n)
			for i := len(s) - 3; i > 0; i -= 3 {
				s = s[:i] + "," + s[i:]
			}
			return s
		},
	}).ParseFS(files, "templates/*.html"))
	return &Server{store: s, baseURL: strings.TrimRight(baseURL, "/"), tpl: tpl}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /{$}", s.createForm)
	mux.HandleFunc("POST /api/links", s.createAPI)
	mux.HandleFunc("GET /api/links", s.listAPI)
	mux.HandleFunc("GET /api/links/{code}", s.getAPI)
	mux.HandleFunc("GET /{code}", s.redirect)
	mux.HandleFunc("GET /{code}/stats", s.stats)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return securityHeaders(mux)
}

type linkJSON struct {
	Code      string           `json:"code"`
	ShortURL  string           `json:"short_url"`
	URL       string           `json:"url"`
	CreatedAt time.Time        `json:"created_at"`
	Clicks    int64            `json:"clicks"`
	LastClick *time.Time       `json:"last_click,omitempty"`
	Daily     map[string]int64 `json:"daily,omitempty"`
	Referrers map[string]int64 `json:"referrers,omitempty"`
}

func (s *Server) toJSON(r *http.Request, l store.Link) linkJSON {
	return linkJSON{Code: l.Code, ShortURL: s.shortURL(r, l.Code), URL: l.URL, CreatedAt: l.CreatedAt,
		Clicks: l.Clicks, LastClick: l.LastClick, Daily: l.Daily, Referrers: l.Referrers}
}

func (s *Server) shortURL(r *http.Request, code string) string {
	if s.baseURL != "" {
		return s.baseURL + "/" + code
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/" + code
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.renderIndex(w, r, http.StatusOK, "", "", "")
}

func (s *Server) renderIndex(w http.ResponseWriter, r *http.Request, status int, errMsg, url, alias string) {
	links := s.store.List()
	if len(links) > 10 {
		links = links[:10]
	}
	rows := make([]linkJSON, len(links))
	for i, l := range links {
		rows[i] = s.toJSON(r, l)
	}
	total, clicks := s.store.Totals()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = s.tpl.ExecuteTemplate(w, "index.html", map[string]any{
		"Error": errMsg, "URL": url, "Alias": alias,
		"Links": rows, "Total": total, "Clicks": clicks,
	})
}

func (s *Server) createForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.renderIndex(w, r, http.StatusBadRequest, "Could not read the form.", "", "")
		return
	}
	url, alias := r.PostForm.Get("url"), r.PostForm.Get("alias")
	l, err := s.store.Create(url, alias)
	if err != nil {
		s.renderIndex(w, r, statusFor(err), message(err), url, alias)
		return
	}
	s.created.Add(1)
	http.Redirect(w, r, "/"+l.Code+"/stats?created=1", http.StatusSeeOther)
}

func (s *Server) createAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL   string `json:"url"`
		Alias string `json:"alias"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON: {\"url\": \"...\", \"alias\": \"optional\"}")
		return
	}
	l, err := s.store.Create(req.URL, req.Alias)
	if err != nil {
		writeError(w, statusFor(err), message(err))
		return
	}
	s.created.Add(1)
	w.Header().Set("Location", "/api/links/"+l.Code)
	writeJSON(w, http.StatusCreated, s.toJSON(r, l))
}

func (s *Server) listAPI(w http.ResponseWriter, r *http.Request) {
	links := s.store.List()
	out := make([]linkJSON, len(links))
	for i, l := range links {
		out[i] = s.toJSON(r, l)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getAPI(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.Get(r.PathValue("code"))
	if err != nil {
		writeError(w, http.StatusNotFound, message(err))
		return
	}
	writeJSON(w, http.StatusOK, s.toJSON(r, l))
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.Hit(r.PathValue("code"), r.Referer())
	if err != nil {
		s.notFound.Add(1)
		http.Error(w, "short link not found", http.StatusNotFound)
		return
	}
	s.redirects.Add(1)
	w.Header().Set("Cache-Control", "private, max-age=0")
	http.Redirect(w, r, target, http.StatusFound)
}

type bar struct {
	Day    string
	Clicks int64
	Height int // percent of the busiest day
}

type ref struct {
	Host   string
	Clicks int64
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.Get(r.PathValue("code"))
	if err != nil {
		http.Error(w, "short link not found", http.StatusNotFound)
		return
	}
	// Last 14 days, oldest first, including days without clicks.
	today := time.Now().UTC()
	bars := make([]bar, 14)
	var peak int64 = 1
	for i := range bars {
		day := today.AddDate(0, 0, i-13)
		n := l.Daily[day.Format(time.DateOnly)]
		bars[i] = bar{Day: day.Format("02/01"), Clicks: n}
		if n > peak {
			peak = n
		}
	}
	for i := range bars {
		bars[i].Height = int(bars[i].Clicks * 100 / peak)
	}
	refs := make([]ref, 0, len(l.Referrers))
	for h, n := range l.Referrers {
		refs = append(refs, ref{h, n})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Clicks == refs[j].Clicks {
			return refs[i].Host < refs[j].Host
		}
		return refs[i].Clicks > refs[j].Clicks
	})
	if len(refs) > 5 {
		refs = refs[:5]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = s.tpl.ExecuteTemplate(w, "stats.html", map[string]any{
		"Link":    s.toJSON(r, l),
		"Created": r.URL.Query().Get("created") == "1",
		"Bars":    bars,
		"Refs":    refs,
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	links, clicks := s.store.Totals()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP shortener_links Short links stored.")
	fmt.Fprintln(w, "# TYPE shortener_links gauge")
	fmt.Fprintf(w, "shortener_links %d\n", links)
	fmt.Fprintln(w, "# HELP shortener_clicks Clicks recorded across all links (persisted).")
	fmt.Fprintln(w, "# TYPE shortener_clicks gauge")
	fmt.Fprintf(w, "shortener_clicks %d\n", clicks)
	fmt.Fprintln(w, "# HELP shortener_links_created_total Links created since start.")
	fmt.Fprintln(w, "# TYPE shortener_links_created_total counter")
	fmt.Fprintf(w, "shortener_links_created_total %d\n", s.created.Load())
	fmt.Fprintln(w, "# HELP shortener_redirects_total Redirects served since start.")
	fmt.Fprintln(w, "# TYPE shortener_redirects_total counter")
	fmt.Fprintf(w, "shortener_redirects_total %d\n", s.redirects.Load())
	fmt.Fprintln(w, "# HELP shortener_not_found_total Requests for unknown codes since start.")
	fmt.Fprintln(w, "# TYPE shortener_not_found_total counter")
	fmt.Fprintf(w, "shortener_not_found_total %d\n", s.notFound.Load())
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrAliasTaken):
		return http.StatusConflict
	case errors.Is(err, store.ErrInvalidURL), errors.Is(err, store.ErrInvalidAlias):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func message(err error) string {
	if statusFor(err) == http.StatusInternalServerError {
		return "internal error"
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
