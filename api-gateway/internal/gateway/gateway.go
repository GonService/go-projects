// Package gateway is the request path: request IDs, route matching, API key
// authentication, per-client rate limiting, load balancing over healthy
// upstreams, access logs and metrics.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GonService/go-projects/api-gateway/internal/config"
	"github.com/GonService/go-projects/api-gateway/internal/metrics"
	"github.com/GonService/go-projects/api-gateway/internal/ratelimit"
	"github.com/GonService/go-projects/api-gateway/internal/upstream"
)

// RequestIDHeader is read from clients (when valid) and sent to upstreams
// and back in the response.
const RequestIDHeader = "X-Request-ID"

// ClientHeader tells upstreams which API key (by name) made the request.
const ClientHeader = "X-Gateway-Client"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// Route is a configured route with its runtime state.
type Route struct {
	Config  config.Route
	Pool    *upstream.Pool
	Limiter *ratelimit.Limiter // nil uses the gateway-wide limiter
	proxies map[*upstream.Target]*httputil.ReverseProxy
}

// Gateway is an http.Handler for the public listener.
type Gateway struct {
	cfg     *config.Config
	routes  []*Route // longest prefix first, for matching
	ordered []*Route // configuration order, for display
	keys    map[[32]byte]string
	limiter *ratelimit.Limiter
	log     *slog.Logger
	started time.Time

	requests    *metrics.Counter
	duration    *metrics.Histogram
	rateLimited *metrics.Counter
	authFailed  *metrics.Counter
	upstreamErr *metrics.Counter

	recentMu sync.Mutex
	recent   []Entry // ring buffer, newest last
}

// Entry is one access log line, kept for the status page.
type Entry struct {
	Time      time.Time     `json:"time"`
	RequestID string        `json:"request_id"`
	Client    string        `json:"client"`
	Method    string        `json:"method"`
	Path      string        `json:"path"`
	Route     string        `json:"route"`
	Status    int           `json:"status"`
	Bytes     int64         `json:"bytes"`
	Duration  time.Duration `json:"duration_ns"`
	Upstream  string        `json:"upstream,omitempty"`
}

const recentSize = 15

// New builds the gateway from a validated configuration.
func New(cfg *config.Config, log *slog.Logger) *Gateway {
	g := &Gateway{
		cfg:         cfg,
		keys:        map[[32]byte]string{},
		limiter:     ratelimit.New(cfg.RateLimit.RPS, cfg.RateLimit.Burst),
		log:         log,
		started:     time.Now(),
		requests:    metrics.NewCounter("gateway_requests_total", "Requests handled, by route and status code."),
		duration:    metrics.NewHistogram("gateway_request_duration_seconds", "Request duration including the upstream.", []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}),
		rateLimited: metrics.NewCounter("gateway_rate_limited_total", "Requests rejected by the rate limiter."),
		authFailed:  metrics.NewCounter("gateway_auth_failures_total", "Requests rejected for a missing or invalid API key."),
		upstreamErr: metrics.NewCounter("gateway_upstream_errors_total", "Proxied requests that failed to reach the upstream."),
	}
	for _, k := range cfg.APIKeys {
		g.keys[sha256.Sum256([]byte(k.Key))] = k.Name
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.UpstreamWait.Duration
	for _, rc := range cfg.Routes {
		hc := rc.HealthCheck
		r := &Route{
			Config: rc,
			Pool: upstream.NewPool(rc.Targets, upstream.CheckConfig{Path: hc.Path, Interval: hc.Interval.Duration,
				Timeout: hc.Timeout.Duration, UnhealthyAfter: hc.UnhealthyAfter, HealthyAfter: hc.HealthyAfter}),
			proxies: map[*upstream.Target]*httputil.ReverseProxy{},
		}
		if rc.RateLimit != nil {
			r.Limiter = ratelimit.New(rc.RateLimit.RPS, rc.RateLimit.Burst)
		}
		for _, t := range r.Pool.Targets {
			r.proxies[t] = g.newProxy(r, t, transport)
		}
		g.routes = append(g.routes, r)
	}
	g.ordered = append([]*Route(nil), g.routes...)
	sort.SliceStable(g.routes, func(i, j int) bool { return len(g.routes[i].Config.Prefix) > len(g.routes[j].Config.Prefix) })
	return g
}

// Routes returns the routes, longest prefix first.
func (g *Gateway) Routes() []*Route { return g.routes }

// RunHealthChecks checks every route's upstreams until ctx is cancelled.
func (g *Gateway) RunHealthChecks(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range g.routes {
		wg.Add(1)
		go func(p *upstream.Pool) { defer wg.Done(); p.Run(ctx) }(r.Pool)
	}
	wg.Wait()
}

func (g *Gateway) newProxy(r *Route, t *upstream.Target, transport http.RoundTripper) *httputil.ReverseProxy {
	strip := r.Config.StripPrefix
	prefix := strings.TrimRight(r.Config.Prefix, "/")
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			if strip {
				pr.Out.URL.Path = ensureSlash(strings.TrimPrefix(pr.Out.URL.Path, prefix))
				if pr.Out.URL.RawPath != "" {
					pr.Out.URL.RawPath = ensureSlash(strings.TrimPrefix(pr.Out.URL.RawPath, prefix))
				}
			}
			pr.SetURL(t.URL)
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				w.WriteHeader(499) // client went away; not the upstream's fault
				return
			}
			r.Pool.ReportFailure(t, err)
			g.upstreamErr.Inc(metrics.Labels("route", r.Config.Name, "target", t.URL.String()))
			writeError(w, http.StatusBadGateway, "upstream unavailable", req.Header.Get(RequestIDHeader))
		},
	}
}

func ensureSlash(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// ServeHTTP handles one public request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	id := req.Header.Get(RequestIDHeader)
	if !validRequestID.MatchString(id) {
		id = newRequestID()
	}
	req.Header.Set(RequestIDHeader, id)
	w.Header().Set(RequestIDHeader, id)
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}

	entry := Entry{Time: start, RequestID: id, Method: req.Method, Path: req.URL.Path, Route: "-"}
	client := g.clientIP(req)
	entry.Client = client
	defer func() {
		entry.Status, entry.Bytes, entry.Duration = rec.status, rec.bytes, time.Since(start)
		g.finish(entry)
	}()

	route := g.match(req.URL.Path)
	if route == nil {
		writeError(rec, http.StatusNotFound, "no route for this path", id)
		return
	}
	entry.Route = route.Config.Name
	labels := metrics.Labels("route", route.Config.Name)

	limitKey := "ip:" + client
	if !route.Config.Public {
		name, ok := g.authenticate(req)
		if !ok {
			g.authFailed.Inc(labels)
			rec.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			writeError(rec, http.StatusUnauthorized, "missing or invalid API key", id)
			return
		}
		entry.Client = name
		limitKey = "key:" + name
		req.Header.Set(ClientHeader, name)
	} else {
		req.Header.Del(ClientHeader) // clients cannot impersonate a key
	}

	limiter := route.Limiter
	if limiter == nil {
		limiter = g.limiter
	}
	res := limiter.Allow(limitKey)
	rec.Header().Set("X-RateLimit-Limit", strconv.Itoa(res.Limit))
	rec.Header().Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
	if !res.Allowed {
		g.rateLimited.Inc(labels)
		rec.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(res.RetryAfter.Seconds()))))
		writeError(rec, http.StatusTooManyRequests, "rate limit exceeded", id)
		return
	}

	target := route.Pool.Next()
	if target == nil {
		writeError(rec, http.StatusServiceUnavailable, "no healthy upstream", id)
		return
	}
	entry.Upstream = target.URL.Host
	route.proxies[target].ServeHTTP(rec, req)
}

// match returns the route with the longest prefix that matches path on a
// segment boundary (/api matches /api and /api/x, not /apix).
func (g *Gateway) match(path string) *Route {
	for _, r := range g.routes {
		p := r.Config.Prefix
		if p == "/" || path == p || strings.HasPrefix(path, strings.TrimRight(p, "/")+"/") {
			return r
		}
	}
	return nil
}

// authenticate accepts "X-API-Key: <key>" or "Authorization: Bearer <key>"
// and removes the key so it never reaches the upstream.
func (g *Gateway) authenticate(req *http.Request) (string, bool) {
	key := req.Header.Get("X-API-Key")
	if key != "" {
		req.Header.Del("X-API-Key")
	} else if auth := req.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		key = strings.TrimPrefix(auth, "Bearer ")
		req.Header.Del("Authorization")
	}
	if key == "" {
		return "", false
	}
	name, ok := g.keys[sha256.Sum256([]byte(key))]
	return name, ok
}

func (g *Gateway) clientIP(req *http.Request) string {
	if g.cfg.TrustProxy {
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func (g *Gateway) finish(e Entry) {
	g.requests.Inc(metrics.Labels("route", e.Route, "code", strconv.Itoa(e.Status)))
	g.duration.Observe(metrics.Labels("route", e.Route), e.Duration.Seconds())
	level := slog.LevelInfo
	if e.Status >= 500 {
		level = slog.LevelWarn
	}
	g.log.LogAttrs(context.Background(), level, "request",
		slog.String("request_id", e.RequestID),
		slog.String("client", e.Client),
		slog.String("method", e.Method),
		slog.String("path", e.Path),
		slog.String("route", e.Route),
		slog.Int("status", e.Status),
		slog.Int64("bytes", e.Bytes),
		slog.Float64("duration_ms", float64(e.Duration.Microseconds())/1000),
		slog.String("upstream", e.Upstream),
	)
	g.recentMu.Lock()
	g.recent = append(g.recent, e)
	if len(g.recent) > recentSize {
		g.recent = g.recent[len(g.recent)-recentSize:]
	}
	g.recentMu.Unlock()
}

// Recent returns the latest requests, newest first.
func (g *Gateway) Recent() []Entry {
	g.recentMu.Lock()
	defer g.recentMu.Unlock()
	out := make([]Entry, len(g.recent))
	for i, e := range g.recent {
		out[len(out)-1-i] = e
	}
	return out
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeError(w http.ResponseWriter, status int, msg, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "request_id": id})
}

// recorder captures the status code and size for logs and metrics.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush lets streaming responses (server-sent events) through.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap gives http.ResponseController access to the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
