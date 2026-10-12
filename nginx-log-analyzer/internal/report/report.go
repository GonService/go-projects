// Package report renders an analysis as a terminal table, JSON or a
// self-contained HTML page (inline CSS and SVG, no external requests).
package report

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GonService/go-projects/nginx-log-analyzer/internal/analyze"
)

//go:embed templates/report.html
var files embed.FS

var tpl = template.Must(template.New("report.html").Funcs(template.FuncMap{
	"num": func(v any) string {
		switch n := v.(type) {
		case int:
			return Num(n)
		case int64:
			return Num(n)
		}
		return fmt.Sprint(v)
	},
	"pct":   func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
	"dur":   Duration,
	"bytes": Bytes,
	"clock": func(t time.Time) string { return t.Format("15:04") },
	"date":  func(t time.Time) string { return t.Format("02/01/2006 15:04:05") },
	"width": func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
	"class": func(code int) string { return fmt.Sprintf("s%d", code/100) },
	"join":  func(s []string) string { return strings.Join(s, ", ") },
}).ParseFS(files, "templates/report.html"))

// JSON writes the report as indented JSON.
func JSON(w io.Writer, r analyze.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// HTML writes a standalone HTML report.
func HTML(w io.Writer, r analyze.Report) error {
	return tpl.Execute(w, map[string]any{
		"R":       r,
		"Chart":   buildChart(r.PerMinute, 900, 220, 120),
		"Minutes": len(r.PerMinute),
	})
}

// Table writes a plain-text report for terminals.
func Table(w io.Writer, r analyze.Report) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	p := func(format string, a ...any) { fmt.Fprintf(tw, format, a...) }

	p("NGINX ACCESS LOG REPORT\n\n")
	p("Sources\t%s\n", strings.Join(r.Sources, ", "))
	if r.Requests > 0 {
		p("Period\t%s to %s (%d min)\n", r.From.Format("02/01/2006 15:04:05"), r.To.Format("02/01/2006 15:04:05 -0700"), len(r.PerMinute))
	}
	p("Requests\t%s (malformed lines skipped: %s)\n", Num(r.Requests), Num(r.Malformed))
	p("Unique IPs\t%s\n", Num(r.UniqueIPs))
	p("Transferred\t%s\n", Bytes(r.Bytes))
	p("Status\t2xx %s · 3xx %s · 4xx %s · 5xx %s (error rate %.1f%%)\n",
		Num(r.Classes["2xx"]), Num(r.Classes["3xx"]), Num(r.Classes["4xx"]), Num(r.Classes["5xx"]), r.ErrorRate)
	if r.PeakMinute != nil {
		p("Requests/min\taverage %.1f, peak %s at %s\n", r.AvgPerMin, Num(r.PeakMinute.Total), r.PeakMinute.Time.Format("15:04"))
	}
	if r.HasTimes {
		p("Latency\tp50 %s · p95 %s\n", Duration(r.P50), Duration(r.P95))
	}

	p("\nTOP PATHS\n")
	p("  REQUESTS\tSHARE\tPATH\n")
	for _, c := range r.TopPaths {
		p("  %s\t%.1f%%\t%s\n", Num(c.Count), c.Percent, c.Key)
	}

	p("\nSTATUS CODES\n")
	p("  CODE\tREQUESTS\tSHARE\n")
	for _, s := range r.Status {
		p("  %d\t%s\t%.1f%%\n", s.Code, Num(s.Count), s.Percent)
	}

	if r.HasTimes {
		p("\nSLOWEST ENDPOINTS (by average $request_time)\n")
		p("  AVG\tP95\tMAX\tREQUESTS\tENDPOINT\n")
		for _, e := range r.Slowest {
			p("  %s\t%s\t%s\t%s\t%s\n", Duration(e.Avg), Duration(e.P95), Duration(e.Max), Num(e.Count), e.Endpoint)
		}
	}

	p("\nTOP IPS\n")
	p("  REQUESTS\tSHARE\tIP\n")
	for _, c := range r.TopIPs {
		p("  %s\t%.1f%%\t%s\n", Num(c.Count), c.Percent, c.Key)
	}

	p("\nTOP USER AGENTS\n")
	p("  REQUESTS\tSHARE\tUSER AGENT\n")
	for _, c := range r.TopAgents {
		p("  %s\t%.1f%%\t%s\n", Num(c.Count), c.Percent, truncate(c.Key, 90))
	}

	p("\nBUSIEST MINUTES\n")
	p("  MINUTE\tREQUESTS\t4XX\t5XX\n")
	for _, m := range busiest(r.PerMinute, 5) {
		p("  %s\t%s\t%s\t%s\n", m.Time.Format("02/01 15:04"), Num(m.Total), Num(m.Err4xx), Num(m.Err5xx))
	}

	p("\nERROR SPIKES\n")
	if len(r.Spikes) == 0 {
		p("  none\n")
	} else {
		p("  MINUTE\tCLASS\tERRORS\tOF REQUESTS\tUSUAL/MIN\n")
		for _, s := range r.Spikes {
			p("  %s\t%s\t%s\t%s\t%.0f\n", s.Time.Format("02/01 15:04"), s.Class, Num(s.Count), Num(s.Total), s.Baseline)
		}
	}
	return tw.Flush()
}

func busiest(mins []analyze.Minute, n int) []analyze.Minute {
	out := append([]analyze.Minute(nil), mins...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Num formats an integer with thousands separators.
func Num[T int | int64](n T) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// Duration formats seconds as "42 ms" or "1.25 s".
func Duration(sec float64) string {
	if sec < 1 {
		return fmt.Sprintf("%.0f ms", sec*1000)
	}
	return fmt.Sprintf("%.2f s", sec)
}

// Bytes formats a byte count with binary units.
func Bytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// chart is a precomputed stacked bar chart of requests over time.
type chart struct {
	W, H, Left, Top, PlotW, PlotH int
	Bars                          []chartBar
	XLabels                       []chartLabel
	YLabels                       []chartLabel
	Bucket                        string
}

type chartBar struct {
	X, W               float64
	OkY, OkH           float64
	E4Y, E4H, E5Y, E5H float64
	Title              string
}

type chartLabel struct {
	X, Y float64
	Text string
}

// buildChart groups minutes into at most maxBars buckets and lays them out
// in SVG coordinates.
func buildChart(mins []analyze.Minute, w, h, maxBars int) chart {
	c := chart{W: w, H: h, Left: 44, Top: 10}
	c.PlotW, c.PlotH = w-c.Left-8, h-c.Top-26
	if len(mins) == 0 {
		return c
	}
	size := (len(mins) + maxBars - 1) / maxBars
	c.Bucket = "1 minute"
	if size > 1 {
		c.Bucket = fmt.Sprintf("%d minutes", size)
	}
	var groups []analyze.Minute
	for i := 0; i < len(mins); i += size {
		g := analyze.Minute{Time: mins[i].Time}
		for _, m := range mins[i:min(i+size, len(mins))] {
			g.Total += m.Total
			g.Err4xx += m.Err4xx
			g.Err5xx += m.Err5xx
		}
		groups = append(groups, g)
	}
	peak := 1
	for _, g := range groups {
		peak = max(peak, g.Total)
	}
	peak = niceCeil(peak)
	slot := float64(c.PlotW) / float64(len(groups))
	gap := min(2.0, slot*0.2)
	scale := float64(c.PlotH) / float64(peak)
	base := float64(c.Top + c.PlotH)
	for i, g := range groups {
		b := chartBar{X: float64(c.Left) + float64(i)*slot + gap/2, W: slot - gap}
		ok := g.Total - g.Err4xx - g.Err5xx
		b.E5H = float64(g.Err5xx) * scale
		b.E5Y = base - b.E5H
		b.E4H = float64(g.Err4xx) * scale
		b.E4Y = b.E5Y - b.E4H
		b.OkH = float64(ok) * scale
		b.OkY = b.E4Y - b.OkH
		b.Title = fmt.Sprintf("%s · %s requests · %s 4xx · %s 5xx", g.Time.Format("15:04"), Num(g.Total), Num(g.Err4xx), Num(g.Err5xx))
		c.Bars = append(c.Bars, b)
	}
	step := max(1, len(groups)/6)
	for i := 0; i < len(groups); i += step {
		c.XLabels = append(c.XLabels, chartLabel{X: float64(c.Left) + (float64(i)+0.5)*slot, Y: base + 17, Text: groups[i].Time.Format("15:04")})
	}
	for i := 0; i <= 4; i++ {
		v := peak * i / 4
		c.YLabels = append(c.YLabels, chartLabel{X: float64(c.Left - 8), Y: base - float64(v)*scale, Text: Num(v)})
	}
	return c
}

// niceCeil rounds n up to a value divisible by 4 (2, 4, 6, 8 or 10 times a
// power of ten) so the four grid lines get round labels.
func niceCeil(n int) int {
	if n <= 4 {
		return 4
	}
	p := 1
	for p*10 < n {
		p *= 10
	}
	for _, m := range []int{2, 4, 6, 8, 10} {
		if v := m * p; v >= n && v%4 == 0 {
			return v
		}
	}
	return 20 * p
}
