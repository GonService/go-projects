// Package check connects to TLS endpoints and inspects their certificates:
// days until expiry, issuer, names, chain validity and hostname match.
package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status of one endpoint, from best to worst.
type Status string

// Possible statuses.
const (
	OK       Status = "ok"
	Warning  Status = "warning"
	Critical Status = "critical"
	Error    Status = "error" // could not connect or complete the handshake
)

func (s Status) rank() int {
	switch s {
	case OK:
		return 0
	case Warning:
		return 1
	case Critical:
		return 2
	default:
		return 3
	}
}

// Worst returns the most severe status in results (OK for none).
func Worst(results []Result) Status {
	w := OK
	for _, r := range results {
		if r.Status.rank() > w.rank() {
			w = r.Status
		}
	}
	return w
}

// Target is an endpoint to check.
type Target struct {
	Input      string `json:"input"`       // as given by the user
	Address    string `json:"address"`     // host:port to connect to
	ServerName string `json:"server_name"` // SNI and name the certificate must match
}

// ParseTarget accepts "host", "host:port", "[ipv6]:port" and
// "name@host:port" (connect to host:port but send and verify "name", useful
// behind load balancers or for checking a single backend).
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	t := Target{Input: s}
	if s == "" {
		return t, errors.New("empty target")
	}
	addr := s
	if name, rest, ok := strings.Cut(s, "@"); ok {
		t.ServerName, addr = name, rest
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port given.
		host, port = strings.Trim(addr, "[]"), "443"
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return t, fmt.Errorf("invalid port in %q", s)
	}
	if host == "" || strings.ContainsAny(host, " /") {
		return t, fmt.Errorf("invalid host in %q", s)
	}
	t.Address = net.JoinHostPort(host, port)
	if t.ServerName == "" {
		t.ServerName = host
	}
	return t, nil
}

// Options control how checks run.
type Options struct {
	Timeout  time.Duration
	Workers  int
	WarnDays int
	CritDays int
	Roots    *x509.CertPool // nil uses the system roots
	Now      func() time.Time
}

// Result is the outcome for one target.
type Result struct {
	Target        Target        `json:"target"`
	Status        Status        `json:"status"`
	Problems      []string      `json:"problems,omitempty"`
	Error         string        `json:"error,omitempty"`
	DaysLeft      int           `json:"days_left"`
	NotBefore     time.Time     `json:"not_before"`
	NotAfter      time.Time     `json:"not_after"`
	Subject       string        `json:"subject,omitempty"`
	Issuer        string        `json:"issuer,omitempty"`
	SANs          []string      `json:"sans,omitempty"`
	Serial        string        `json:"serial,omitempty"`
	ChainValid    bool          `json:"chain_valid"`
	ChainError    string        `json:"chain_error,omitempty"`
	ChainLength   int           `json:"chain_length"`
	HostnameMatch bool          `json:"hostname_match"`
	TLSVersion    string        `json:"tls_version,omitempty"`
	Latency       time.Duration `json:"latency_ns"`
	CheckedAt     time.Time     `json:"checked_at"`
}

// Connected reports whether a certificate was retrieved.
func (r Result) Connected() bool { return r.Error == "" }

// Run checks all targets with a pool of workers and returns results in the
// same order as targets.
func Run(ctx context.Context, targets []Target, opt Options) []Result {
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	results := make([]Result, len(targets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(opt.Workers, len(targets)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = One(ctx, targets[i], opt)
			}
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

// One checks a single target.
func One(ctx context.Context, t Target, opt Options) Result {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	r := Result{Target: t, CheckedAt: opt.Now()}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	// Verification is done by hand below so that an expired or untrusted
	// certificate is still inspected and reported instead of only failing.
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: opt.Timeout},
		Config: &tls.Config{
			ServerName:         t.ServerName,
			InsecureSkipVerify: true, // verified by hand in inspect
			MinVersion:         tls.VersionTLS10,
		},
	}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", t.Address)
	r.Latency = time.Since(start)
	if err != nil {
		r.Status = Error
		r.Error = shortError(err)
		return r
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	r.TLSVersion = tls.VersionName(state.Version)
	inspect(&r, state.PeerCertificates, opt)
	return r
}

func inspect(r *Result, chain []*x509.Certificate, opt Options) {
	if len(chain) == 0 {
		r.Status = Error
		r.Error = "server sent no certificate"
		return
	}
	leaf := chain[0]
	now := opt.Now()
	r.ChainLength = len(chain)
	r.NotBefore, r.NotAfter = leaf.NotBefore, leaf.NotAfter
	r.DaysLeft = int(math.Floor(leaf.NotAfter.Sub(now).Hours() / 24))
	r.Subject = leaf.Subject.CommonName
	r.Issuer = issuerName(leaf)
	r.Serial = fmt.Sprintf("%X", leaf.SerialNumber)
	r.SANs = append(r.SANs, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		r.SANs = append(r.SANs, ip.String())
	}

	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: opt.Roots, Intermediates: inter, CurrentTime: now})
	r.ChainValid = err == nil
	if err != nil {
		r.ChainError = chainError(err)
	}
	r.HostnameMatch = leaf.VerifyHostname(r.Target.ServerName) == nil

	r.Status = OK
	switch {
	case now.After(leaf.NotAfter):
		r.Problems = append(r.Problems, fmt.Sprintf("expired %d days ago", -r.DaysLeft))
		r.Status = Critical
	case now.Before(leaf.NotBefore):
		r.Problems = append(r.Problems, "not valid yet")
		r.Status = Critical
	case r.DaysLeft < opt.CritDays:
		r.Problems = append(r.Problems, fmt.Sprintf("expires in %d days", r.DaysLeft))
		r.Status = Critical
	case r.DaysLeft < opt.WarnDays:
		r.Problems = append(r.Problems, fmt.Sprintf("expires in %d days", r.DaysLeft))
		r.Status = Warning
	}
	// An expired certificate already explains the chain failure.
	if !r.ChainValid && !now.After(leaf.NotAfter) && !now.Before(leaf.NotBefore) {
		r.Problems = append(r.Problems, r.ChainError)
		r.Status = Critical
	}
	if !r.HostnameMatch {
		r.Problems = append(r.Problems, "certificate is not valid for "+r.Target.ServerName)
		r.Status = Critical
	}
}

func issuerName(c *x509.Certificate) string {
	if c.Issuer.CommonName != "" {
		if len(c.Issuer.Organization) > 0 && c.Issuer.Organization[0] != c.Issuer.CommonName {
			return c.Issuer.Organization[0] + " / " + c.Issuer.CommonName
		}
		return c.Issuer.CommonName
	}
	if len(c.Issuer.Organization) > 0 {
		return c.Issuer.Organization[0]
	}
	return c.Issuer.String()
}

func chainError(err error) string {
	var ua x509.UnknownAuthorityError
	var inv x509.CertificateInvalidError
	switch {
	case errors.As(err, &ua):
		if ua.Cert != nil && ua.Cert.Subject.String() == ua.Cert.Issuer.String() {
			return "self-signed certificate"
		}
		return "issued by an unknown authority (missing intermediate or private CA)"
	case errors.As(err, &inv):
		return inv.Error()
	default:
		return err.Error()
	}
}

func shortError(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return "timeout"
	case strings.Contains(err.Error(), "connection refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "no such host"):
		return "host not found"
	}
	return err.Error()
}
