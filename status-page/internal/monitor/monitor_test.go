package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func server(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
}

func target(name, url string) Target {
	return Target{Name: name, URL: url, Interval: time.Minute, Timeout: time.Second}
}

func TestCheckUpAndDown(t *testing.T) {
	ok, broken := server(200), server(503)
	defer ok.Close()
	defer broken.Close()

	m := New([]Target{target("ok", ok.URL), target("broken", broken.URL)}, 10)
	if r := m.Check(context.Background(), "ok"); !r.Up || r.Status != 200 {
		t.Fatalf("expected up, got %+v", r)
	}
	r := m.Check(context.Background(), "broken")
	if r.Up || r.Status != 503 || r.Error != "unexpected status 503" {
		t.Fatalf("expected down with 503, got %+v", r)
	}
	if got := Overall(m.States()); got != "degraded" {
		t.Fatalf("overall = %q, want degraded", got)
	}
}

func TestExpectedStatus(t *testing.T) {
	s := server(204)
	defer s.Close()
	tg := target("api", s.URL)
	tg.ExpectedStatus = 200
	m := New([]Target{tg}, 10)
	if r := m.Check(context.Background(), "api"); r.Up {
		t.Fatal("204 must be down when 200 is expected")
	}
}

func TestTimeoutAndConnectionErrors(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	tg := target("slow", slow.URL)
	tg.Timeout = 50 * time.Millisecond
	m := New([]Target{tg, target("closed", "http://127.0.0.1:1")}, 10)
	if r := m.Check(context.Background(), "slow"); r.Up || r.Error == "" {
		t.Fatalf("expected timeout error, got %+v", r)
	}
	if r := m.Check(context.Background(), "closed"); r.Up || r.Error == "" {
		t.Fatalf("expected connection error, got %+v", r)
	}
	if got := Overall(m.States()); got != "down" {
		t.Fatalf("overall = %q, want down", got)
	}
}

func TestHistoryIsCappedAndUptimeComputed(t *testing.T) {
	status := 200
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer s.Close()
	m := New([]Target{target("svc", s.URL)}, 4)
	for i := 0; i < 6; i++ {
		if i == 5 {
			status = 500
		}
		m.Check(context.Background(), "svc")
	}
	st := m.States()[0]
	if len(st.History) != 4 {
		t.Fatalf("history length = %d, want 4", len(st.History))
	}
	if st.Uptime != 0.75 {
		t.Fatalf("uptime = %v, want 0.75", st.Uptime)
	}
	if st.TotalChecks != 6 || st.TotalFails != 1 || st.Up {
		t.Fatalf("unexpected counters %+v", st)
	}
}

func TestOverall(t *testing.T) {
	cases := map[string][]State{
		"pending":     {{Checked: false}},
		"operational": {{Checked: true, Up: true}, {Checked: false}},
		"down":        {{Checked: true, Up: false}},
	}
	for want, states := range cases {
		if got := Overall(states); got != want {
			t.Errorf("Overall = %q, want %q", got, want)
		}
	}
}

func TestRunChecksImmediatelyAndStops(t *testing.T) {
	s := server(200)
	defer s.Close()
	m := New([]Target{target("svc", s.URL)}, 5)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !m.States()[0].Checked && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if !m.States()[0].Up {
		t.Fatal("first check should run immediately and succeed")
	}
}
