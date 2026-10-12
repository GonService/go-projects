package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GonService/go-projects/ssl-expiry-checker/internal/testcerts"
)

func serveTLS(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s.Listener.Addr().String()
}

func days(n int) time.Time { return time.Now().Add(time.Duration(n)*24*time.Hour + time.Hour) }

func target(t *testing.T, s string) Target {
	t.Helper()
	tg, err := ParseTarget(s)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestCertificateStates(t *testing.T) {
	ca, err := testcerts.NewCA("Test Root CA")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := testcerts.NewCA("Someone Else CA")
	issue := func(c *testcerts.CA, l testcerts.Leaf) tls.Certificate {
		cert, err := c.Issue(l)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	selfSigned, err := testcerts.SelfSigned(testcerts.Leaf{Names: []string{"localhost"}, NotAfter: days(100)})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		cert    tls.Certificate
		sni     string
		status  Status
		days    int
		chain   bool
		host    bool
		problem string
	}{
		{"valid", issue(ca, testcerts.Leaf{Names: []string{"localhost", "127.0.0.1"}, NotAfter: days(200)}), "localhost", OK, 200, true, true, ""},
		{"warning", issue(ca, testcerts.Leaf{Names: []string{"localhost"}, NotAfter: days(20)}), "localhost", Warning, 20, true, true, "expires in 20 days"},
		{"critical", issue(ca, testcerts.Leaf{Names: []string{"localhost"}, NotAfter: days(3)}), "localhost", Critical, 3, true, true, "expires in 3 days"},
		{"expired", issue(ca, testcerts.Leaf{Names: []string{"localhost"}, NotBefore: days(-90), NotAfter: days(-3)}), "localhost", Critical, -3, false, true, "expired 3 days ago"},
		{"self-signed", selfSigned, "localhost", Critical, 100, false, true, "self-signed certificate"},
		{"unknown CA", issue(other, testcerts.Leaf{Names: []string{"localhost"}, NotAfter: days(100)}), "localhost", Critical, 100, false, true, "unknown authority"},
		{"hostname mismatch", issue(ca, testcerts.Leaf{Names: []string{"api.example.com"}, NotAfter: days(100)}), "www.example.com", Critical, 100, true, false, "not valid for www.example.com"},
		{"wildcard", issue(ca, testcerts.Leaf{Names: []string{"*.example.com"}, NotAfter: days(100)}), "shop.example.com", OK, 100, true, true, ""},
	}
	opt := Options{Timeout: 3 * time.Second, WarnDays: 30, CritDays: 7, Roots: ca.Pool()}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr := serveTLS(t, c.cert)
			r := One(context.Background(), target(t, c.sni+"@"+addr), opt)
			if r.Error != "" {
				t.Fatalf("error: %s", r.Error)
			}
			if r.Status != c.status || r.DaysLeft != c.days || r.ChainValid != c.chain || r.HostnameMatch != c.host {
				t.Fatalf("got status=%s days=%d chain=%v host=%v problems=%v chainErr=%q",
					r.Status, r.DaysLeft, r.ChainValid, r.HostnameMatch, r.Problems, r.ChainError)
			}
			if c.problem != "" && !strings.Contains(strings.Join(r.Problems, "; "), c.problem) {
				t.Fatalf("problems %v do not mention %q", r.Problems, c.problem)
			}
			if c.problem == "" && len(r.Problems) != 0 {
				t.Fatalf("unexpected problems: %v", r.Problems)
			}
			if r.TLSVersion == "" || r.ChainLength != 1 || r.Subject == "" {
				t.Fatalf("missing details: %+v", r)
			}
		})
	}
}

func TestIssuerAndSANs(t *testing.T) {
	ca, _ := testcerts.NewCA("Acme Issuing CA")
	cert, _ := ca.Issue(testcerts.Leaf{Names: []string{"localhost", "www.localhost", "127.0.0.1"}, NotAfter: days(60)})
	addr := serveTLS(t, cert)
	r := One(context.Background(), target(t, "localhost@"+addr), Options{WarnDays: 30, CritDays: 7, Roots: ca.Pool()})
	if r.Issuer != "Acme Issuing CA" || strings.Join(r.SANs, ",") != "localhost,www.localhost,127.0.0.1" {
		t.Fatalf("issuer = %q sans = %v", r.Issuer, r.SANs)
	}
	if r.Status != OK {
		t.Fatalf("status = %s %v", r.Status, r.Problems)
	}
}

// httptest.NewTLSServer comes with its own certificate for example.com and
// 127.0.0.1; trusting it makes the endpoint pass.
func TestHTTPTestDefaultCertificate(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	addr := ts.Listener.Addr().String()

	r := One(context.Background(), target(t, "example.com@"+addr), Options{WarnDays: 30, CritDays: 7, Roots: pool})
	if r.Status != OK || !r.ChainValid || !r.HostnameMatch {
		t.Fatalf("trusted: %+v", r)
	}
	// Without the pool the same server is untrusted.
	r = One(context.Background(), target(t, "example.com@"+addr), Options{WarnDays: 30, CritDays: 7, Roots: x509.NewCertPool()})
	if r.Status != Critical || r.ChainValid {
		t.Fatalf("untrusted: %+v", r)
	}
}

func TestConnectionErrors(t *testing.T) {
	// Refused: a port that was just closed.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	r := One(context.Background(), target(t, closed), Options{Timeout: time.Second})
	if r.Status != Error || r.Error != "connection refused" || r.Connected() {
		t.Fatalf("refused: %+v", r)
	}

	// Timeout: accepts TCP but never answers the handshake.
	silent, _ := net.Listen("tcp", "127.0.0.1:0")
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start := time.Now()
	r = One(context.Background(), target(t, silent.Addr().String()), Options{Timeout: 300 * time.Millisecond})
	if r.Status != Error || r.Error != "timeout" {
		t.Fatalf("timeout: %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not honoured")
	}
}

func TestRunKeepsOrderAndRunsInParallel(t *testing.T) {
	ca, _ := testcerts.NewCA("Test CA")
	var targets []Target
	for i := 0; i < 12; i++ {
		cert, _ := ca.Issue(testcerts.Leaf{Names: []string{"localhost"}, NotAfter: days(10 + i*10)})
		targets = append(targets, target(t, "localhost@"+serveTLS(t, cert)))
	}
	results := Run(context.Background(), targets, Options{Workers: 4, WarnDays: 30, CritDays: 7, Roots: ca.Pool()})
	for i, r := range results {
		if r.Target != targets[i] || r.DaysLeft != 10+i*10 {
			t.Fatalf("result %d: target=%v days=%d", i, r.Target, r.DaysLeft)
		}
	}
	if Worst(results) != Warning {
		t.Fatalf("worst = %s", Worst(results))
	}
	if Worst(nil) != OK || Worst([]Result{{Status: OK}, {Status: Error}, {Status: Critical}}) != Error {
		t.Fatal("Worst ordering")
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct{ in, addr, name string }{
		{"example.com", "example.com:443", "example.com"},
		{"example.com:8443", "example.com:8443", "example.com"},
		{" api.example.com:443 ", "api.example.com:443", "api.example.com"},
		{"www.example.com@10.0.0.5:443", "10.0.0.5:443", "www.example.com"},
		{"[2001:db8::1]:443", "[2001:db8::1]:443", "2001:db8::1"},
		{"2001:db8::1", "[2001:db8::1]:443", "2001:db8::1"},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.in)
		if err != nil || got.Address != c.addr || got.ServerName != c.name {
			t.Errorf("ParseTarget(%q) = %+v, %v", c.in, got, err)
		}
	}
	for _, bad := range []string{"", "example.com:0", "example.com:http", "bad host:443", "https://example.com/"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) accepted", bad)
		}
	}
}
