// Package report renders check results as a terminal table, JSON, an HTML
// page and Prometheus metrics.
package report

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GonService/go-projects/ssl-expiry-checker/internal/check"
)

//go:embed templates/report.html
var files embed.FS

var tpl = template.Must(template.New("report.html").Funcs(template.FuncMap{
	"date":  func(t time.Time) string { return t.Format("2006-01-02") },
	"stamp": func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") },
	"join":  func(s []string) string { return strings.Join(s, ", ") },
	"ms":    func(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) },
	// bar maps days left to a 0-100 width; 90 days or more is a full bar.
	"bar": func(days int) int { return max(0, min(100, days*100/90)) },
}).ParseFS(files, "templates/report.html"))

// Meta describes a run for the report header.
type Meta struct {
	Generated time.Time
	WarnDays  int
	CritDays  int
	Refresh   int // seconds; 0 disables the auto-refresh (static report)
}

// Summary counts results per status.
type Summary struct {
	OK, Warning, Critical, Error int
	Worst                        check.Status
}

// Summarize counts results per status.
func Summarize(results []check.Result) Summary {
	s := Summary{Worst: check.Worst(results)}
	for _, r := range results {
		switch r.Status {
		case check.OK:
			s.OK++
		case check.Warning:
			s.Warning++
		case check.Critical:
			s.Critical++
		default:
			s.Error++
		}
	}
	return s
}

// HTML writes a standalone HTML report.
func HTML(w io.Writer, results []check.Result, m Meta) error {
	return tpl.Execute(w, map[string]any{"Results": results, "Meta": m, "Summary": Summarize(results)})
}

// JSON writes the results with a summary.
func JSON(w io.Writer, results []check.Result, m Meta) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	s := Summarize(results)
	return enc.Encode(map[string]any{
		"generated": m.Generated,
		"warn_days": m.WarnDays,
		"crit_days": m.CritDays,
		"status":    s.Worst,
		"summary":   map[string]int{"ok": s.OK, "warning": s.Warning, "critical": s.Critical, "error": s.Error},
		"results":   results,
	})
}

// Table writes an aligned plain-text table.
func Table(w io.Writer, results []check.Result, m Meta) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tTARGET\tDAYS\tEXPIRES\tISSUER\tCHAIN\tHOST\tNOTES")
	for _, r := range results {
		if !r.Connected() {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t-\t%s\n", strings.ToUpper(string(r.Status)), r.Target.Input, r.Error)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			strings.ToUpper(string(r.Status)), r.Target.Input, r.DaysLeft, r.NotAfter.Format("2006-01-02"),
			truncate(r.Issuer, 32), yesNo(r.ChainValid, "valid", "invalid"), yesNo(r.HostnameMatch, "match", "MISMATCH"),
			strings.Join(r.Problems, "; "))
	}
	s := Summarize(results)
	fmt.Fprintf(tw, "\n%d checked: %d ok, %d warning, %d critical, %d error (warn < %d days, critical < %d days)\n",
		len(results), s.OK, s.Warning, s.Critical, s.Error, m.WarnDays, m.CritDays)
	return tw.Flush()
}

// Metrics writes the results in the Prometheus text format.
func Metrics(w io.Writer, results []check.Result) {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace
	labels := func(r check.Result) string {
		return fmt.Sprintf(`target="%s",server_name="%s"`, esc(r.Target.Address), esc(r.Target.ServerName))
	}
	gauge := func(name, help string, value func(check.Result) (float64, bool)) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, r := range results {
			if v, ok := value(r); ok {
				fmt.Fprintf(w, "%s{%s} %s\n", name, labels(r), strconv.FormatFloat(v, 'f', -1, 64))
			}
		}
	}
	b := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	gauge("ssl_check_success", "Whether the TLS handshake succeeded (1) or not (0).", func(r check.Result) (float64, bool) {
		return b(r.Connected()), true
	})
	gauge("ssl_cert_expiry_days", "Days until the certificate expires (negative when expired).", func(r check.Result) (float64, bool) {
		return float64(r.DaysLeft), r.Connected()
	})
	gauge("ssl_cert_not_after_timestamp_seconds", "Certificate expiry as a Unix timestamp.", func(r check.Result) (float64, bool) {
		return float64(r.NotAfter.Unix()), r.Connected()
	})
	gauge("ssl_cert_chain_valid", "Whether the chain verifies against the trusted roots.", func(r check.Result) (float64, bool) {
		return b(r.ChainValid), r.Connected()
	})
	gauge("ssl_cert_hostname_match", "Whether the certificate is valid for the server name.", func(r check.Result) (float64, bool) {
		return b(r.HostnameMatch), r.Connected()
	})
	gauge("ssl_check_duration_seconds", "Time to connect and complete the TLS handshake.", func(r check.Result) (float64, bool) {
		return r.Latency.Seconds(), true
	})
}

func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
