package analyze

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/GonService/go-projects/nginx-log-analyzer/internal/logparse"
)

var t0 = time.Date(2026, 10, 12, 14, 0, 0, 0, time.FixedZone("", -3*3600))

func entry(min int, ip, method, path string, status int, rt float64) logparse.Entry {
	return logparse.Entry{IP: ip, Time: t0.Add(time.Duration(min) * time.Minute), Method: method, Path: path,
		Status: status, Bytes: 100, UserAgent: "test", RequestTime: rt, HasTime: rt > 0}
}

func TestReportCounts(t *testing.T) {
	a := New(DefaultOptions())
	a.Add(entry(0, "1.1.1.1", "GET", "/users/1", 200, 0.1))
	a.Add(entry(0, "1.1.1.1", "GET", "/users/2", 200, 0.3))
	a.Add(entry(2, "2.2.2.2", "GET", "/", 404, 0.01))
	a.Add(entry(3, "3.3.3.3", "POST", "/orders", 502, 2))
	a.Malformed()

	r := a.Report([]string{"x.log"})
	if r.Requests != 4 || r.Malformed != 1 || r.UniqueIPs != 3 || r.Bytes != 400 {
		t.Fatalf("totals: %+v", r)
	}
	if r.TopPaths[0].Key != "/users/:id" || r.TopPaths[0].Count != 2 || r.TopPaths[0].Percent != 50 {
		t.Errorf("top path = %+v", r.TopPaths[0])
	}
	if r.Classes["2xx"] != 2 || r.Classes["4xx"] != 1 || r.Classes["5xx"] != 1 || r.ErrorRate != 50 {
		t.Errorf("classes = %v rate = %v", r.Classes, r.ErrorRate)
	}
	// Minutes 0..3 are filled, including the empty minute 1.
	if len(r.PerMinute) != 4 || r.PerMinute[1].Total != 0 || r.PerMinute[0].Total != 2 {
		t.Errorf("per minute = %+v", r.PerMinute)
	}
	if r.PeakMinute.Total != 2 || r.AvgPerMin != 1 {
		t.Errorf("peak = %+v avg = %v", r.PeakMinute, r.AvgPerMin)
	}
	if !r.HasTimes || r.Slowest[0].Endpoint != "POST /orders" || r.Slowest[0].Max != 2 {
		t.Errorf("slowest = %+v", r.Slowest)
	}
	if got := r.Slowest[1]; got.Endpoint != "GET /users/:id" || math.Abs(got.Avg-0.2) > 1e-9 || got.Count != 2 {
		t.Errorf("second slowest = %+v", got)
	}
}

func TestSpikes(t *testing.T) {
	a := New(Options{Top: 5, SpikeMin: 5, SpikeX: 3})
	for m := 0; m < 30; m++ {
		for i := 0; i < 20; i++ {
			a.Add(entry(m, "10.0.0.1", "GET", "/", 200, 0))
		}
		a.Add(entry(m, "10.0.0.1", "GET", "/missing", 404, 0)) // 1 per minute is normal
	}
	for i := 0; i < 12; i++ {
		a.Add(entry(10, "10.0.0.9", "GET", "/.env", 404, 0))
		a.Add(entry(20, "10.0.0.2", "GET", "/pay", 503, 0))
	}
	for i := 0; i < 3; i++ {
		a.Add(entry(25, "10.0.0.2", "GET", "/pay", 500, 0)) // below SpikeMin
	}
	r := a.Report(nil)
	if len(r.Spikes) != 2 {
		t.Fatalf("spikes = %+v", r.Spikes)
	}
	if s := r.Spikes[0]; s.Class != "4xx" || s.Count != 13 || s.Baseline != 1 || !s.Time.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("first spike = %+v", s)
	}
	if s := r.Spikes[1]; s.Class != "5xx" || s.Count != 12 {
		t.Errorf("second spike = %+v", s)
	}
	if r.HasTimes || r.Slowest != nil {
		t.Error("no request_time in input, but latency reported")
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/":                  "/",
		"/users/42":          "/users/:id",
		"/users/42/orders/7": "/users/:id/orders/:id",
		"/v2/items":          "/v2/items",
		"/files/3f2b9c1e-8a4d-4b6e-9f00-1234567890ab": "/files/:id",
		"/blob/9f86d081884c7d659a2feaa0c55ad015":      "/blob/:id",
		"/static/app.1234.js":                         "/static/app.1234.js",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuantileAccuracy(t *testing.T) {
	l := &latency{}
	for i := 1; i <= 1000; i++ {
		l.add(float64(i) / 1000) // 1ms .. 1s, uniform
	}
	for _, c := range []struct{ q, want float64 }{{0.5, 0.5}, {0.95, 0.95}, {0.99, 0.99}} {
		got := l.quantile(c.q)
		if math.Abs(got-c.want)/c.want > 0.1 {
			t.Errorf("q%.2f = %v, want %v within 10%%", c.q, got, c.want)
		}
	}
	if l.quantile(1) != 1 {
		t.Errorf("max quantile = %v", l.quantile(1))
	}
}

func TestTopIsStableAndLimited(t *testing.T) {
	a := New(Options{Top: 3})
	for i := 0; i < 5; i++ {
		a.Add(entry(0, fmt.Sprintf("10.0.0.%d", i), "GET", "/", 200, 0))
	}
	r := a.Report(nil)
	if len(r.TopIPs) != 3 || r.TopIPs[0].Key != "10.0.0.0" || r.TopIPs[2].Key != "10.0.0.2" {
		t.Errorf("top ips = %+v", r.TopIPs)
	}
}
