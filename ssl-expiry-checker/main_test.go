package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GonService/go-projects/ssl-expiry-checker/internal/testcerts"
)

type env struct {
	caFile string
	addrs  map[string]string // name -> host:port
}

func setup(t *testing.T, leaves map[string]int) env {
	t.Helper()
	ca, err := testcerts.NewCA("Test Root CA")
	if err != nil {
		t.Fatal(err)
	}
	e := env{caFile: filepath.Join(t.TempDir(), "ca.pem"), addrs: map[string]string{}}
	if err := os.WriteFile(e.caFile, ca.PEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, daysLeft := range leaves {
		cert, err := ca.Issue(testcerts.Leaf{Names: []string{"localhost"}, NotAfter: time.Now().Add(time.Duration(daysLeft)*24*time.Hour + time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewUnstartedServer(http.NotFoundHandler())
		s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		s.StartTLS()
		t.Cleanup(s.Close)
		e.addrs[name] = "localhost@" + s.Listener.Addr().String()
	}
	return e
}

func runCLI(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestExitCodes(t *testing.T) {
	e := setup(t, map[string]int{"good": 120, "soon": 20, "urgent": 2})
	cases := []struct {
		targets []string
		want    int
	}{
		{[]string{e.addrs["good"]}, exitOK},
		{[]string{e.addrs["good"], e.addrs["soon"]}, exitWarning},
		{[]string{e.addrs["good"], e.addrs["soon"], e.addrs["urgent"]}, exitCritical},
		{[]string{e.addrs["good"], "localhost@127.0.0.1:1"}, exitCritical}, // unreachable
	}
	for _, c := range cases {
		args := append([]string{"-ca-file", e.caFile, "-timeout", "2s"}, c.targets...)
		if code, out, errOut := runCLI(args...); code != c.want {
			t.Errorf("%v: exit %d, want %d\n%s%s", c.targets, code, c.want, out, errOut)
		}
	}
	// Without the private CA the chain is untrusted: critical.
	if code, out, _ := runCLI(e.addrs["good"]); code != exitCritical || !strings.Contains(out, "unknown authority") {
		t.Errorf("untrusted: exit %d\n%s", code, out)
	}
	// Thresholds are configurable.
	if code, _, _ := runCLI("-ca-file", e.caFile, "-warn", "10", "-crit", "5", e.addrs["soon"]); code != exitOK {
		t.Errorf("custom thresholds: exit %d", code)
	}
}

func TestTargetsFileAndJSON(t *testing.T) {
	e := setup(t, map[string]int{"a": 90, "b": 40})
	list := "# production endpoints\n" + e.addrs["a"] + "\n\n" + e.addrs["b"] + "   # api\n"
	file := filepath.Join(t.TempDir(), "targets.txt")
	if err := os.WriteFile(file, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("-ca-file", e.caFile, "-f", file, "-format", "json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var doc struct {
		Status  string `json:"status"`
		Results []struct {
			DaysLeft int  `json:"days_left"`
			Chain    bool `json:"chain_valid"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "ok" || len(doc.Results) != 2 || doc.Results[0].DaysLeft != 90 || doc.Results[1].DaysLeft != 40 || !doc.Results[0].Chain {
		t.Fatalf("json: %s", out)
	}
}

func TestHTMLFile(t *testing.T) {
	e := setup(t, map[string]int{"a": 15})
	path := filepath.Join(t.TempDir(), "report.html")
	code, _, _ := runCLI("-ca-file", e.caFile, "-format", "html", "-o", path, e.addrs["a"])
	if code != exitWarning {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(b, []byte("expires in 15 days")) {
		t.Fatalf("report: %v\n%s", err, b)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-format", "xml", "example.com"},
		{"-warn", "5", "-crit", "10", "example.com"},
		{"bad host:99999"},
		{"-f", "missing.txt"},
		{"-ca-file", "missing.pem", "example.com"},
		{"-no-such-flag"},
	} {
		if code, _, _ := runCLI(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
