package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GonService/go-projects/ssl-expiry-checker/internal/check"
)

func results() []check.Result {
	exp := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	return []check.Result{
		{Target: check.Target{Input: "a.example.com", Address: "a.example.com:443", ServerName: "a.example.com"}, Status: check.OK,
			DaysLeft: 95, NotAfter: exp, Issuer: "Test CA", SANs: []string{"a.example.com"}, ChainValid: true, ChainLength: 2, HostnameMatch: true, TLSVersion: "TLS 1.3", Latency: 12 * time.Millisecond},
		{Target: check.Target{Input: "b.example.com", Address: "b.example.com:443", ServerName: `b"<x>`}, Status: check.Critical,
			DaysLeft: -2, NotAfter: exp, Issuer: "Test CA", Problems: []string{"expired 2 days ago"}, ChainLength: 1, HostnameMatch: true},
		{Target: check.Target{Input: "down.example.com", Address: "down.example.com:443", ServerName: "down.example.com"}, Status: check.Error, Error: "timeout"},
	}
}

func TestMetrics(t *testing.T) {
	var b bytes.Buffer
	Metrics(&b, results())
	out := b.String()
	for _, want := range []string{
		`ssl_cert_expiry_days{target="a.example.com:443",server_name="a.example.com"} 95`,
		`ssl_cert_expiry_days{target="b.example.com:443",server_name="b\"<x>"} -2`,
		`ssl_check_success{target="down.example.com:443",server_name="down.example.com"} 0`,
		`ssl_cert_chain_valid{target="a.example.com:443",server_name="a.example.com"} 1`,
		`ssl_cert_not_after_timestamp_seconds{target="a.example.com:443",server_name="a.example.com"} 1799971200`,
		"# TYPE ssl_cert_expiry_days gauge",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `ssl_cert_expiry_days{target="down.example.com:443"`) {
		t.Error("unreachable endpoint should not export expiry days")
	}
}

func TestTableJSONHTML(t *testing.T) {
	m := Meta{Generated: time.Now(), WarnDays: 30, CritDays: 7}
	var tb bytes.Buffer
	if err := Table(&tb, results(), m); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"STATUS", "OK", "CRITICAL", "expired 2 days ago", "timeout", "3 checked: 1 ok, 0 warning, 1 critical, 1 error"} {
		if !strings.Contains(tb.String(), want) {
			t.Errorf("table missing %q:\n%s", want, tb.String())
		}
	}

	var jb bytes.Buffer
	if err := JSON(&jb, results(), m); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Status  string         `json:"status"`
		Summary map[string]int `json:"summary"`
		Results []any          `json:"results"`
	}
	if err := json.Unmarshal(jb.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "error" || doc.Summary["ok"] != 1 || len(doc.Results) != 3 {
		t.Errorf("json = %s", jb.String())
	}

	var hb bytes.Buffer
	if err := HTML(&hb, results(), m); err != nil {
		t.Fatal(err)
	}
	html := hb.String()
	if strings.Contains(html, `b"<x>`) || !strings.Contains(html, "b&#34;&lt;x&gt;") {
		t.Error("server name not escaped")
	}
	if strings.Contains(html, "http-equiv=\"refresh\"") {
		t.Error("static report should not auto-refresh")
	}
	for _, want := range []string{"TLS certificate report", "expired 2 days ago", "timeout", "TLS 1.3"} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}
}
