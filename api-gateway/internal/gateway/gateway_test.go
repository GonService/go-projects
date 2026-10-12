package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GonService/go-projects/api-gateway/internal/config"
)

const (
	mobileKey  = "mobile-key-0123456789"
	partnerKey = "partner-key-0123456789"
)

// echo is an upstream that reports what it received.
type echo struct {
	name string
	srv  *httptest.Server
}

type seen struct {
	Upstream  string `json:"upstream"`
	Path      string `json:"path"`
	Query     string `json:"query"`
	RequestID string `json:"request_id"`
	Client    string `json:"client"`
	APIKey    string `json:"api_key"`
	Auth      string `json:"auth"`
	XFF       string `json:"xff"`
}

func newEcho(t *testing.T, name string) *echo {
	t.Helper()
	e := &echo{name: name}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			return
		}
		_ = json.NewEncoder(w).Encode(seen{Upstream: name, Path: r.URL.Path, Query: r.URL.RawQuery,
			RequestID: r.Header.Get(RequestIDHeader), Client: r.Header.Get(ClientHeader),
			APIKey: r.Header.Get("X-API-Key"), Auth: r.Header.Get("Authorization"), XFF: r.Header.Get("X-Forwarded-For")})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

type fixture struct {
	gw    *Gateway
	srv   *httptest.Server
	admin *httptest.Server
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func setup(t *testing.T, cfgJSON string) *fixture {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	gw := New(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	f := &fixture{gw: gw, srv: httptest.NewServer(gw), admin: httptest.NewServer(gw.AdminHandler()), logs: logs}
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.admin.Close)
	return f
}

func baseConfig(users1, users2, orders, catalog string) string {
	return fmt.Sprintf(`{
	  "rate_limit": {"rps": 100, "burst": 100},
	  "api_keys": [{"name": "mobile-app", "key": %q}, {"name": "partner", "key": %q}],
	  "routes": [
	    {"name": "users", "prefix": "/api/users", "upstreams": [%q, %q], "health_check": {"unhealthy_after": 1}},
	    {"name": "user-admin", "prefix": "/api/users/admin", "upstreams": [%q]},
	    {"name": "orders", "prefix": "/api/orders", "upstreams": [%q], "rate_limit": {"rps": 0.01, "burst": 2}},
	    {"name": "catalog", "prefix": "/catalog/", "strip_prefix": true, "public": true, "upstreams": [%q]}
	  ]}`, mobileKey, partnerKey, users1, users2, orders, orders, catalog)
}

func do(t *testing.T, method, url string, headers ...string) (*http.Response, seen) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var s seen
	body, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(body, &s)
	return res, s
}

func TestRoutingAuthAndHeaders(t *testing.T) {
	u1, u2, orders, catalog := newEcho(t, "users-1"), newEcho(t, "users-2"), newEcho(t, "orders"), newEcho(t, "catalog")
	f := setup(t, baseConfig(u1.srv.URL, u2.srv.URL, orders.srv.URL, catalog.srv.URL))

	// API key in X-API-Key: forwarded path, client name, key removed.
	res, s := do(t, "GET", f.srv.URL+"/api/users/42?expand=1", "X-API-Key", mobileKey, RequestIDHeader, "abc-123")
	if res.StatusCode != 200 || s.Path != "/api/users/42" || s.Query != "expand=1" || s.Client != "mobile-app" || s.APIKey != "" {
		t.Fatalf("users: %d %+v", res.StatusCode, s)
	}
	if s.RequestID != "abc-123" || res.Header.Get(RequestIDHeader) != "abc-123" || s.XFF != "127.0.0.1" {
		t.Fatalf("headers: %+v / %v", s, res.Header)
	}

	// Bearer token works too, and is not forwarded.
	_, s = do(t, "GET", f.srv.URL+"/api/users", "Authorization", "Bearer "+partnerKey)
	if s.Client != "partner" || s.Auth != "" {
		t.Fatalf("bearer: %+v", s)
	}

	// Longest prefix wins.
	_, s = do(t, "GET", f.srv.URL+"/api/users/admin/roles", "X-API-Key", mobileKey)
	if s.Upstream != "orders" {
		t.Fatalf("longest prefix: %+v", s)
	}

	// Prefix must match on a segment boundary.
	if res, _ := do(t, "GET", f.srv.URL+"/api/usersX", "X-API-Key", mobileKey); res.StatusCode != 404 {
		t.Fatalf("/api/usersX status = %d", res.StatusCode)
	}

	// Public route with strip_prefix; a spoofed client header is removed.
	res, s = do(t, "GET", f.srv.URL+"/catalog/items/9", ClientHeader, "mobile-app")
	if res.StatusCode != 200 || s.Path != "/items/9" || s.Client != "" {
		t.Fatalf("catalog: %d %+v", res.StatusCode, s)
	}

	// Missing, wrong and malformed keys.
	for _, h := range [][]string{nil, {"X-API-Key", "wrong-key-000000000"}, {"Authorization", "Basic " + mobileKey}} {
		res, _ := do(t, "GET", f.srv.URL+"/api/users/1", h...)
		if res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("auth %v: status %d", h, res.StatusCode)
		}
	}

	// An invalid incoming request ID is replaced with a generated one.
	res, s = do(t, "GET", f.srv.URL+"/catalog/x", RequestIDHeader, "bad id with spaces")
	if id := res.Header.Get(RequestIDHeader); len(id) != 24 || s.RequestID != id {
		t.Fatalf("generated id %q, upstream saw %q", id, s.RequestID)
	}

	// Access log has one JSON line per request with the useful fields.
	var line map[string]any
	first := strings.SplitN(f.logs.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &line); err != nil {
		t.Fatal(err)
	}
	if line["request_id"] != "abc-123" || line["client"] != "mobile-app" || line["route"] != "users" || line["status"] != 200.0 || line["upstream"] == "" {
		t.Fatalf("access log: %v", line)
	}
	if strings.Contains(f.logs.String(), mobileKey) {
		t.Fatal("API key leaked into the access log")
	}
}

func TestRoundRobinAndFailover(t *testing.T) {
	u1, u2, other := newEcho(t, "users-1"), newEcho(t, "users-2"), newEcho(t, "other")
	f := setup(t, baseConfig(u1.srv.URL, u2.srv.URL, other.srv.URL, other.srv.URL))

	count := map[string]int{}
	for i := 0; i < 6; i++ {
		_, s := do(t, "GET", f.srv.URL+"/api/users", "X-API-Key", mobileKey)
		count[s.Upstream]++
	}
	if count["users-1"] != 3 || count["users-2"] != 3 {
		t.Fatalf("round robin: %v", count)
	}

	// users-2 dies: its request fails with 502, then it is skipped
	// (unhealthy_after = 1) and every request goes to users-1.
	u2.srv.Close()
	failures := 0
	for i := 0; i < 6; i++ {
		res, s := do(t, "GET", f.srv.URL+"/api/users", "X-API-Key", mobileKey)
		switch {
		case res.StatusCode == 502:
			failures++
		case s.Upstream != "users-1":
			t.Fatalf("request %d went to %q", i, s.Upstream)
		}
	}
	if failures != 1 {
		t.Fatalf("502 responses = %d, want 1", failures)
	}

	// Health checks confirm it; when every target is down the gateway answers 503.
	route := f.gw.Routes()[func() int {
		for i, r := range f.gw.Routes() {
			if r.Config.Name == "users" {
				return i
			}
		}
		return -1
	}()]
	u1.srv.Close()
	route.Pool.CheckAll(context.Background())
	if res, _ := do(t, "GET", f.srv.URL+"/api/users", "X-API-Key", mobileKey); res.StatusCode != 503 {
		t.Fatalf("all down: status %d", res.StatusCode)
	}
}

func TestRateLimitPerClient(t *testing.T) {
	up := newEcho(t, "up")
	f := setup(t, baseConfig(up.srv.URL, up.srv.URL, up.srv.URL, up.srv.URL))

	// orders: burst 2 per API key.
	for i := 0; i < 2; i++ {
		res, _ := do(t, "GET", f.srv.URL+"/api/orders", "X-API-Key", mobileKey)
		if res.StatusCode != 200 || res.Header.Get("X-RateLimit-Limit") != "2" || res.Header.Get("X-RateLimit-Remaining") != fmt.Sprint(1-i) {
			t.Fatalf("request %d: %d %v", i, res.StatusCode, res.Header)
		}
	}
	res, _ := do(t, "GET", f.srv.URL+"/api/orders", "X-API-Key", mobileKey)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "100" {
		t.Fatalf("over limit: %d retry-after %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	// Another key has its own bucket.
	if res, _ := do(t, "GET", f.srv.URL+"/api/orders", "X-API-Key", partnerKey); res.StatusCode != 200 {
		t.Fatalf("partner limited by mobile's usage: %d", res.StatusCode)
	}
	// The limit is per route: users still works for mobile.
	if res, _ := do(t, "GET", f.srv.URL+"/api/users", "X-API-Key", mobileKey); res.StatusCode != 200 {
		t.Fatalf("users: %d", res.StatusCode)
	}
}

func TestRateLimitByIPOnPublicRoutes(t *testing.T) {
	up := newEcho(t, "up")
	f := setup(t, fmt.Sprintf(`{"trust_proxy": true, "rate_limit": {"rps": 0.01, "burst": 1},
	  "routes": [{"name": "pub", "prefix": "/", "public": true, "upstreams": [%q]}]}`, up.srv.URL))
	if res, _ := do(t, "GET", f.srv.URL+"/a", "X-Forwarded-For", "203.0.113.7"); res.StatusCode != 200 {
		t.Fatal("first request limited")
	}
	if res, _ := do(t, "GET", f.srv.URL+"/a", "X-Forwarded-For", "203.0.113.7, 10.0.0.1"); res.StatusCode != 429 {
		t.Fatal("same client IP not limited")
	}
	if res, _ := do(t, "GET", f.srv.URL+"/a", "X-Forwarded-For", "198.51.100.2"); res.StatusCode != 200 {
		t.Fatal("different client IP limited")
	}
}

func TestAdmin(t *testing.T) {
	up := newEcho(t, "users-1")
	f := setup(t, baseConfig(up.srv.URL, up.srv.URL, up.srv.URL, up.srv.URL))
	do(t, "GET", f.srv.URL+"/api/users/1", "X-API-Key", mobileKey)
	do(t, "GET", f.srv.URL+"/api/users/1")
	do(t, "GET", f.srv.URL+"/nowhere")

	page := get(t, f.admin.URL+"/")
	for _, want := range []string{"API gateway", "/api/users", "user-admin", "catalog", "Healthy", "GET /api/users/1", "mobile-app"} {
		if !strings.Contains(page, want) {
			t.Errorf("status page missing %q", want)
		}
	}
	var st Status
	if err := json.Unmarshal([]byte(get(t, f.admin.URL+"/api/status")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Requests != 3 || st.AuthFailed != 1 || len(st.Routes) != 4 || st.Upstreams != 5 || len(st.Recent) != 3 || st.Recent[0].Path != "/nowhere" {
		t.Fatalf("status: %+v", st)
	}
	metrics := get(t, f.admin.URL+"/metrics")
	for _, want := range []string{
		`gateway_requests_total{route="users",code="200"} 1`,
		`gateway_requests_total{route="users",code="401"} 1`,
		`gateway_requests_total{route="-",code="404"} 1`,
		`gateway_auth_failures_total{route="users"} 1`,
		`gateway_request_duration_seconds_count{route="users"} 2`,
		`gateway_upstream_up{route="users",target="` + up.srv.URL + `"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("metrics missing %q:\n%s", want, metrics)
		}
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, res.StatusCode)
	}
	return string(b)
}
