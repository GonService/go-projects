package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GonService/go-projects/status-page/internal/monitor"
)

type fakeSource []monitor.State

func (f fakeSource) States() []monitor.State { return f }

func sample() fakeSource {
	now := time.Now()
	up := monitor.Result{At: now, Up: true, Status: 200, Latency: 120 * time.Millisecond}
	down := monitor.Result{At: now, Up: false, Error: "unexpected status 502", Latency: 30 * time.Millisecond}
	return fakeSource{
		{Target: monitor.Target{Name: "Website"}, Checked: true, Up: true, Last: &up, Uptime: 1, History: []monitor.Result{up}, TotalChecks: 1},
		{Target: monitor.Target{Name: `API "v2"`}, Checked: true, Up: false, Last: &down, Uptime: 0.5, History: []monitor.Result{up, down}, TotalChecks: 2, TotalFails: 1},
	}
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Result(), string(body)
}

func TestStatusPage(t *testing.T) {
	res, body := get(t, Handler("Acme status", sample()), "/")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	for _, want := range []string{"Acme status", "Some services are having problems", "Website", "unexpected status 502", "50.00% uptime"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("missing security headers")
	}
}

func TestAPI(t *testing.T) {
	_, body := get(t, Handler("Acme", sample()), "/api/status")
	var out struct {
		Status   string `json:"status"`
		Services []struct {
			Up bool `json:"up"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "degraded" || len(out.Services) != 2 || !out.Services[0].Up {
		t.Fatalf("unexpected API response %+v", out)
	}
}

func TestMetricsEscapeLabels(t *testing.T) {
	_, body := get(t, Handler("Acme", sample()), "/metrics")
	for _, want := range []string{
		`status_page_up{service="Website"} 1`,
		`status_page_up{service="API \"v2\""} 0`,
		`status_page_failures_total{service="API \"v2\""} 1`,
		`status_page_latency_seconds{service="Website"} 0.120`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestUnknownPathIs404(t *testing.T) {
	if res, _ := get(t, Handler("Acme", sample()), "/nope"); res.StatusCode != 404 {
		t.Fatalf("status %d, want 404", res.StatusCode)
	}
}
