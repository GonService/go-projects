package server

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GonService/go-projects/ssl-expiry-checker/internal/check"
	"github.com/GonService/go-projects/ssl-expiry-checker/internal/testcerts"
)

func TestDaemonEndpoints(t *testing.T) {
	ca, _ := testcerts.NewCA("Test CA")
	cert, _ := ca.Issue(testcerts.Leaf{Names: []string{"localhost"}, NotAfter: time.Now().Add(45*24*time.Hour + time.Hour)})
	backend := httptest.NewUnstartedServer(http.NotFoundHandler())
	backend.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	backend.StartTLS()
	defer backend.Close()

	target, _ := check.ParseTarget("localhost@" + backend.Listener.Addr().String())
	s := New([]check.Target{target}, check.Options{Timeout: 2 * time.Second, WarnDays: 30, CritDays: 7, Roots: ca.Pool()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if res, _ := http.Get(ts.URL + "/healthz"); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz before first run = %d", res.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if res, err := http.Get(ts.URL + "/healthz"); err == nil && res.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first check did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	metrics := body(t, ts.URL+"/metrics")
	want := `ssl_cert_expiry_days{target="` + target.Address + `",server_name="localhost"} 45`
	if !strings.Contains(metrics, want+"\n") {
		t.Fatalf("metrics missing %q:\n%s", want, metrics)
	}
	page := body(t, ts.URL+"/")
	if !strings.Contains(page, "http-equiv=\"refresh\"") || !strings.Contains(page, ">45<") {
		t.Fatalf("html page:\n%s", page)
	}
	if js := body(t, ts.URL+"/api/results"); !strings.Contains(js, `"days_left": 45`) {
		t.Fatalf("json:\n%s", js)
	}
}

func body(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}
