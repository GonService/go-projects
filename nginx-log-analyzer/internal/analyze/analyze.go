// Package analyze aggregates parsed log entries into a report. Memory grows
// with the number of distinct paths, IPs, user agents and minutes, not with
// the number of lines, so large files can be streamed through it.
package analyze

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GonService/go-projects/nginx-log-analyzer/internal/logparse"
)

// Options tune the analysis.
type Options struct {
	Top       int  // rows in each "top" list
	Normalize bool // collapse IDs in paths: /users/42 -> /users/:id
	SpikeMin  int  // minimum errors in a minute to call it a spike
	SpikeX    float64
}

// DefaultOptions are used by the CLI unless overridden by flags.
func DefaultOptions() Options {
	return Options{Top: 10, Normalize: true, SpikeMin: 10, SpikeX: 3}
}

// Analyzer accumulates entries. It is not safe for concurrent use.
type Analyzer struct {
	opt       Options
	total     int
	malformed int
	bytes     int64
	first     time.Time
	last      time.Time
	loc       *time.Location
	paths     map[string]int
	status    map[int]int
	ips       map[string]int
	agents    map[string]int
	minutes   map[int64]*minute
	latency   map[string]*latency
	timed     int
	allTimes  *latency
}

type minute struct{ total, c4xx, c5xx int }

// New returns an empty Analyzer.
func New(opt Options) *Analyzer {
	if opt.Top <= 0 {
		opt.Top = 10
	}
	if opt.SpikeX <= 0 {
		opt.SpikeX = 3
	}
	return &Analyzer{
		opt:      opt,
		paths:    map[string]int{},
		status:   map[int]int{},
		ips:      map[string]int{},
		agents:   map[string]int{},
		minutes:  map[int64]*minute{},
		latency:  map[string]*latency{},
		allTimes: &latency{},
	}
}

// Malformed records a line that could not be parsed.
func (a *Analyzer) Malformed() { a.malformed++ }

// Add records one entry.
func (a *Analyzer) Add(e logparse.Entry) {
	a.total++
	a.bytes += e.Bytes
	if a.loc == nil {
		a.loc = e.Time.Location()
	}
	if a.first.IsZero() || e.Time.Before(a.first) {
		a.first = e.Time
	}
	if e.Time.After(a.last) {
		a.last = e.Time
	}
	path := e.Path
	if a.opt.Normalize {
		path = NormalizePath(path)
	}
	a.paths[path]++
	a.status[e.Status]++
	a.ips[e.IP]++
	ua := e.UserAgent
	if ua == "" {
		ua = "(none)"
	}
	a.agents[ua]++

	key := e.Time.Unix() / 60
	m := a.minutes[key]
	if m == nil {
		m = &minute{}
		a.minutes[key] = m
	}
	m.total++
	switch {
	case e.Status >= 500:
		m.c5xx++
	case e.Status >= 400:
		m.c4xx++
	}

	if e.HasTime {
		a.timed++
		a.allTimes.add(e.RequestTime)
		ep := e.Method + " " + path
		l := a.latency[ep]
		if l == nil {
			l = &latency{}
			a.latency[ep] = l
		}
		l.add(e.RequestTime)
	}
}

// Count is a value with its number of occurrences.
type Count struct {
	Key     string  `json:"key"`
	Count   int     `json:"count"`
	Percent float64 `json:"percent"`
}

// StatusCount is one status code.
type StatusCount struct {
	Code    int     `json:"code"`
	Count   int     `json:"count"`
	Percent float64 `json:"percent"`
}

// Minute is the traffic in one minute.
type Minute struct {
	Time   time.Time `json:"time"`
	Total  int       `json:"total"`
	Err4xx int       `json:"4xx"`
	Err5xx int       `json:"5xx"`
}

// Endpoint is the latency of one method + path.
type Endpoint struct {
	Endpoint string  `json:"endpoint"`
	Count    int     `json:"count"`
	Avg      float64 `json:"avg_seconds"`
	P95      float64 `json:"p95_seconds"`
	Max      float64 `json:"max_seconds"`
}

// Spike is a minute with unusually many errors of one class.
type Spike struct {
	Time     time.Time `json:"time"`
	Class    string    `json:"class"` // "4xx" or "5xx"
	Count    int       `json:"count"`
	Total    int       `json:"total"`
	Baseline float64   `json:"baseline"` // median errors per minute of that class
}

// Report is the result of an analysis.
type Report struct {
	Sources    []string       `json:"sources"`
	Generated  time.Time      `json:"generated"`
	Requests   int            `json:"requests"`
	Malformed  int            `json:"malformed_lines"`
	Bytes      int64          `json:"bytes"`
	UniqueIPs  int            `json:"unique_ips"`
	From       time.Time      `json:"from"`
	To         time.Time      `json:"to"`
	Classes    map[string]int `json:"status_classes"`
	ErrorRate  float64        `json:"error_rate"`
	Status     []StatusCount  `json:"status_codes"`
	TopPaths   []Count        `json:"top_paths"`
	TopIPs     []Count        `json:"top_ips"`
	TopAgents  []Count        `json:"top_user_agents"`
	PerMinute  []Minute       `json:"requests_per_minute"`
	PeakMinute *Minute        `json:"peak_minute,omitempty"`
	AvgPerMin  float64        `json:"avg_requests_per_minute"`
	HasTimes   bool           `json:"has_request_time"`
	P50        float64        `json:"p50_seconds,omitempty"`
	P95        float64        `json:"p95_seconds,omitempty"`
	Slowest    []Endpoint     `json:"slowest_endpoints,omitempty"`
	Spikes     []Spike        `json:"spikes"`
}

// Report builds the report. sources only labels the output.
func (a *Analyzer) Report(sources []string) Report {
	r := Report{
		Sources:   sources,
		Generated: time.Now(),
		Requests:  a.total,
		Malformed: a.malformed,
		Bytes:     a.bytes,
		UniqueIPs: len(a.ips),
		From:      a.first,
		To:        a.last,
		Classes:   map[string]int{"2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0},
		HasTimes:  a.timed > 0,
		Spikes:    []Spike{},
	}
	errs := 0
	for code, n := range a.status {
		class := strconv.Itoa(code/100) + "xx"
		r.Classes[class] += n
		if code >= 400 {
			errs += n
		}
		r.Status = append(r.Status, StatusCount{Code: code, Count: n, Percent: pct(n, a.total)})
	}
	sort.Slice(r.Status, func(i, j int) bool { return r.Status[i].Code < r.Status[j].Code })
	r.ErrorRate = pct(errs, a.total)
	r.TopPaths = top(a.paths, a.opt.Top, a.total)
	r.TopIPs = top(a.ips, a.opt.Top, a.total)
	r.TopAgents = top(a.agents, a.opt.Top, a.total)

	// Fill every minute between the first and last one so gaps show as zero.
	if len(a.minutes) > 0 {
		start, end := a.first.Unix()/60, a.last.Unix()/60
		for k := start; k <= end; k++ {
			m := Minute{Time: time.Unix(k*60, 0).In(a.loc)}
			if v := a.minutes[k]; v != nil {
				m.Total, m.Err4xx, m.Err5xx = v.total, v.c4xx, v.c5xx
			}
			r.PerMinute = append(r.PerMinute, m)
		}
		peak := r.PerMinute[0]
		for _, m := range r.PerMinute {
			if m.Total > peak.Total {
				peak = m
			}
		}
		r.PeakMinute = &peak
		r.AvgPerMin = float64(a.total) / float64(len(r.PerMinute))
		r.Spikes = spikes(r.PerMinute, a.opt)
	}

	if a.timed > 0 {
		r.P50 = a.allTimes.quantile(0.50)
		r.P95 = a.allTimes.quantile(0.95)
		for ep, l := range a.latency {
			r.Slowest = append(r.Slowest, Endpoint{Endpoint: ep, Count: l.n, Avg: l.sum / float64(l.n), P95: l.quantile(0.95), Max: l.max})
		}
		sort.Slice(r.Slowest, func(i, j int) bool {
			if r.Slowest[i].Avg == r.Slowest[j].Avg {
				return r.Slowest[i].Endpoint < r.Slowest[j].Endpoint
			}
			return r.Slowest[i].Avg > r.Slowest[j].Avg
		})
		if len(r.Slowest) > a.opt.Top {
			r.Slowest = r.Slowest[:a.opt.Top]
		}
	}
	return r
}

// spikes flags minutes whose 4xx or 5xx count is at least SpikeMin and more
// than SpikeX times the median for that class. The median is used instead of
// the mean because a big spike would inflate the mean it is compared to.
func spikes(mins []Minute, opt Options) []Spike {
	out := []Spike{}
	for _, class := range []string{"4xx", "5xx"} {
		get := func(m Minute) int {
			if class == "4xx" {
				return m.Err4xx
			}
			return m.Err5xx
		}
		vals := make([]int, len(mins))
		for i, m := range mins {
			vals[i] = get(m)
		}
		sort.Ints(vals)
		median := float64(vals[len(vals)/2])
		if len(vals)%2 == 0 {
			median = float64(vals[len(vals)/2-1]+vals[len(vals)/2]) / 2
		}
		limit := math.Max(float64(opt.SpikeMin), opt.SpikeX*math.Max(median, 1))
		for _, m := range mins {
			if n := get(m); float64(n) >= limit {
				out = append(out, Spike{Time: m.Time, Class: class, Count: n, Total: m.Total, Baseline: median})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time.Equal(out[j].Time) {
			return out[i].Class < out[j].Class
		}
		return out[i].Time.Before(out[j].Time)
	})
	return out
}

func top(m map[string]int, n, total int) []Count {
	out := make([]Count, 0, len(m))
	for k, v := range m {
		out = append(out, Count{Key: k, Count: v, Percent: pct(v, total)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Key < out[j].Key
		}
		return out[i].Count > out[j].Count
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

var (
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexRe  = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
)

// NormalizePath replaces path segments that look like identifiers (numbers,
// UUIDs, long hex strings) with ":id" so /orders/17 and /orders/18 are
// counted as the same endpoint.
func NormalizePath(p string) string {
	if !strings.ContainsAny(p, "0123456789") {
		return p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" {
			continue
		}
		if isDigits(s) || uuidRe.MatchString(s) || hexRe.MatchString(s) {
			segs[i] = ":id"
		}
	}
	return strings.Join(segs, "/")
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// latency keeps count, sum, max and a log-scale histogram, so quantiles can be
// estimated without storing every sample. Bucket i covers values up to
// 1ms * 1.1^i, which keeps the estimate within 10% of the real value.
type latency struct {
	n       int
	sum     float64
	max     float64
	buckets []int
}

const (
	bucketBase   = 0.001
	bucketFactor = 1.1
)

func (l *latency) add(v float64) {
	l.n++
	l.sum += v
	if v > l.max {
		l.max = v
	}
	i := 0
	if v > bucketBase {
		i = int(math.Ceil(math.Log(v/bucketBase) / math.Log(bucketFactor)))
	}
	for len(l.buckets) <= i {
		l.buckets = append(l.buckets, 0)
	}
	l.buckets[i]++
}

func (l *latency) quantile(q float64) float64 {
	if l.n == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(l.n)))
	seen := 0
	for i, c := range l.buckets {
		seen += c
		if seen >= rank {
			return math.Min(bucketBase*math.Pow(bucketFactor, float64(i)), l.max)
		}
	}
	return l.max
}
