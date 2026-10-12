// Package metrics is a minimal Prometheus text-format registry: labelled
// counters and histograms, enough for a gateway without pulling in the
// client library.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var escape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace

// Labels formats label pairs ("route", "users", "code", "200") as
// `route="users",code="200"`.
func Labels(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, kv[i], escape(kv[i+1]))
	}
	return b.String()
}

// Counter is a monotonically increasing value per label set.
type Counter struct {
	Name, Help string
	mu         sync.Mutex
	values     map[string]float64
}

// NewCounter returns an empty counter.
func NewCounter(name, help string) *Counter {
	return &Counter{Name: name, Help: help, values: map[string]float64{}}
}

// Inc adds one for the label set.
func (c *Counter) Inc(labels string) { c.Add(labels, 1) }

// Add adds v for the label set.
func (c *Counter) Add(labels string, v float64) {
	c.mu.Lock()
	c.values[labels] += v
	c.mu.Unlock()
}

// Get returns the value for the label set.
func (c *Counter) Get(labels string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[labels]
}

// Sum returns the total over all label sets whose labels contain filter.
func (c *Counter) Sum(filter string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t float64
	for k, v := range c.values {
		if strings.Contains(k, filter) {
			t += v
		}
	}
	return t
}

// Write emits the counter in text format.
func (c *Counter) Write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.Name, c.Help, c.Name)
	for _, k := range sortedKeys(c.values) {
		fmt.Fprintf(w, "%s{%s} %s\n", c.Name, k, num(c.values[k]))
	}
}

// Histogram counts observations into cumulative buckets per label set.
type Histogram struct {
	Name, Help string
	Buckets    []float64
	mu         sync.Mutex
	series     map[string]*hseries
}

type hseries struct {
	counts []uint64
	sum    float64
	count  uint64
}

// NewHistogram returns a histogram with the given upper bounds.
func NewHistogram(name, help string, buckets []float64) *Histogram {
	return &Histogram{Name: name, Help: help, Buckets: buckets, series: map[string]*hseries{}}
}

// Observe records v for the label set.
func (h *Histogram) Observe(labels string, v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[labels]
	if s == nil {
		s = &hseries{counts: make([]uint64, len(h.Buckets))}
		h.series[labels] = s
	}
	for i, b := range h.Buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

// Write emits the histogram in text format.
func (h *Histogram) Write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.Name, h.Help, h.Name)
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		sep := ""
		if k != "" {
			sep = ","
		}
		for i, b := range h.Buckets {
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"%s\"} %d\n", h.Name, k, sep, num(b), s.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", h.Name, k, sep, s.count)
		fmt.Fprintf(w, "%s_sum{%s} %s\n", h.Name, k, num(s.sum))
		fmt.Fprintf(w, "%s_count{%s} %d\n", h.Name, k, s.count)
	}
}

// Gauge writes a gauge family computed at scrape time.
func Gauge(w io.Writer, name, help string, values map[string]float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	for _, k := range sortedKeys(values) {
		fmt.Fprintf(w, "%s{%s} %s\n", name, k, num(values[k]))
	}
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
