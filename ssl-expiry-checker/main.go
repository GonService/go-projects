// Command ssl-expiry-checker checks TLS certificates of many endpoints at
// once. As a CLI it prints a report and exits with a status code for CI; in
// daemon mode it rechecks on a schedule and exposes Prometheus metrics.
package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GonService/go-projects/ssl-expiry-checker/internal/check"
	"github.com/GonService/go-projects/ssl-expiry-checker/internal/report"
	"github.com/GonService/go-projects/ssl-expiry-checker/internal/server"
)

// Exit codes.
const (
	exitOK       = 0
	exitWarning  = 1
	exitCritical = 2 // includes endpoints that could not be checked
	exitUsage    = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ssl-expiry-checker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "", "file with one target per line (host, host:port or name@host:port; # starts a comment)")
	warn := fs.Int("warn", 30, "warning when a certificate expires in fewer days than this")
	crit := fs.Int("crit", 7, "critical when a certificate expires in fewer days than this")
	timeout := fs.Duration("timeout", 5*time.Second, "connect and handshake timeout per endpoint")
	workers := fs.Int("workers", 8, "endpoints checked in parallel")
	format := fs.String("format", "table", "output format: table, json or html")
	out := fs.String("o", "", "write the report to this file instead of stdout")
	caFile := fs.String("ca-file", "", "PEM file with extra trusted CAs (for private PKI)")
	daemon := fs.Bool("daemon", false, "keep running: recheck every -interval and serve /metrics")
	addr := fs.String("addr", ":9115", "listen address in daemon mode")
	interval := fs.Duration("interval", time.Hour, "time between checks in daemon mode")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ssl-expiry-checker [flags] [target ...]")
		fmt.Fprintln(stderr, "Exit codes: 0 ok, 1 warning, 2 critical or unreachable, 3 usage error.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "ssl-expiry-checker:", err)
		return exitUsage
	}
	if *crit > *warn {
		return fail(errors.New("-crit must not be greater than -warn"))
	}
	render, ok := map[string]func(io.Writer, []check.Result, report.Meta) error{
		"table": report.Table, "json": report.JSON, "html": report.HTML,
	}[*format]
	if !ok {
		return fail(fmt.Errorf("unknown format %q (use table, json or html)", *format))
	}

	inputs := fs.Args()
	if *file != "" {
		lines, err := readTargets(*file)
		if err != nil {
			return fail(err)
		}
		inputs = append(inputs, lines...)
	}
	if len(inputs) == 0 {
		fs.Usage()
		return exitUsage
	}
	targets := make([]check.Target, 0, len(inputs))
	for _, in := range inputs {
		t, err := check.ParseTarget(in)
		if err != nil {
			return fail(err)
		}
		targets = append(targets, t)
	}

	opt := check.Options{Timeout: *timeout, Workers: *workers, WarnDays: *warn, CritDays: *crit}
	if *caFile != "" {
		pool, err := loadRoots(*caFile)
		if err != nil {
			return fail(err)
		}
		opt.Roots = pool
	}

	if *daemon {
		return serve(ctx, targets, opt, *addr, *interval, stderr)
	}

	results := check.Run(ctx, targets, opt)
	meta := report.Meta{Generated: time.Now(), WarnDays: *warn, CritDays: *crit}
	if err := write(*out, stdout, func(w io.Writer) error { return render(w, results, meta) }); err != nil {
		return fail(err)
	}
	switch check.Worst(results) {
	case check.OK:
		return exitOK
	case check.Warning:
		return exitWarning
	default:
		return exitCritical
	}
}

func serve(ctx context.Context, targets []check.Target, opt check.Options, addr string, every time.Duration, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	s := server.New(targets, opt)
	go s.Run(ctx, every)
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", addr, "targets", len(targets), "interval", every.String())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "error", err)
		return exitUsage
	}
	return exitOK
}

func write(path string, stdout io.Writer, render func(io.Writer) error) error {
	if path == "" {
		return render(stdout)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := render(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readTargets(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func loadRoots(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no PEM certificates found", path)
	}
	return pool, nil
}
