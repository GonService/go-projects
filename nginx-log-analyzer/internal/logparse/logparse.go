// Package logparse parses Nginx access log lines in the "combined" format,
// optionally followed by $request_time:
//
//	$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent
//	"$http_referer" "$http_user_agent" [$request_time | rt=$request_time ...]
//
// The parser is hand-written (no regular expressions) because it runs once per
// line on files that can have millions of lines.
package logparse

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// TimeLayout is Nginx's $time_local.
const TimeLayout = "02/Jan/2006:15:04:05 -0700"

// ErrMalformed is returned for lines that are not in the combined format.
var ErrMalformed = errors.New("malformed log line")

// Entry is one parsed request.
type Entry struct {
	IP          string
	Time        time.Time
	Method      string
	Path        string // without the query string
	Protocol    string
	Status      int
	Bytes       int64
	Referer     string
	UserAgent   string
	RequestTime float64 // seconds; valid only when HasTime is true
	HasTime     bool
}

// Parse parses one line.
func Parse(line string) (Entry, error) {
	var e Entry
	line = strings.TrimRight(line, "\r\n")

	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		return e, ErrMalformed
	}
	e.IP = line[:sp]
	rest := line[sp:]

	open := strings.IndexByte(rest, '[')
	end := strings.IndexByte(rest, ']')
	if open < 0 || end < open {
		return e, ErrMalformed
	}
	t, err := time.Parse(TimeLayout, rest[open+1:end])
	if err != nil {
		return e, ErrMalformed
	}
	e.Time = t
	rest = rest[end+1:]

	request, rest, ok := quoted(rest)
	if !ok {
		return e, ErrMalformed
	}
	e.Method, e.Path, e.Protocol = splitRequest(request)

	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return e, ErrMalformed
	}
	if e.Status, err = strconv.Atoi(fields[0]); err != nil || e.Status < 100 || e.Status > 599 {
		return e, ErrMalformed
	}
	if fields[1] != "-" {
		if e.Bytes, err = strconv.ParseInt(fields[1], 10, 64); err != nil {
			return e, ErrMalformed
		}
	}
	// Skip the two numbers and parse the quoted referer and user agent.
	rest = afterFields(rest, 2)
	if e.Referer, rest, ok = quoted(rest); !ok {
		return e, ErrMalformed
	}
	if e.UserAgent, rest, ok = quoted(rest); !ok {
		return e, ErrMalformed
	}
	if e.Referer == "-" {
		e.Referer = ""
	}
	if e.UserAgent == "-" {
		e.UserAgent = ""
	}
	e.RequestTime, e.HasTime = requestTime(rest)
	return e, nil
}

// quoted returns the content of the next double-quoted string in s and what
// follows it. Backslash escapes are honoured.
func quoted(s string) (string, string, bool) {
	start := strings.IndexByte(s, '"')
	if start < 0 {
		return "", s, false
	}
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return s[start+1 : i], s[i+1:], true
		}
	}
	return "", s, false
}

func afterFields(s string, n int) string {
	i := 0
	for ; n > 0; n-- {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		for i < len(s) && s[i] != ' ' {
			i++
		}
	}
	return s[i:]
}

func splitRequest(r string) (method, path, proto string) {
	parts := strings.Fields(r)
	if len(parts) < 2 || !isMethod(parts[0]) {
		// Garbage such as TLS handshakes sent to a plain HTTP port.
		return "-", "(invalid request)", ""
	}
	method, path = parts[0], parts[1]
	if len(parts) > 2 {
		proto = parts[2]
	}
	if q := strings.IndexAny(path, "?#"); q >= 0 {
		path = path[:q]
	}
	if path == "" {
		path = "/"
	}
	return method, path, proto
}

func isMethod(m string) bool {
	if m == "" || len(m) > 10 {
		return false
	}
	for i := 0; i < len(m); i++ {
		if m[i] < 'A' || m[i] > 'Z' {
			return false
		}
	}
	return true
}

// requestTime finds $request_time after the user agent. It accepts a bare
// number ("0.123") or a key=value pair ("rt=0.123", "request_time=0.123").
func requestTime(rest string) (float64, bool) {
	for _, f := range strings.Fields(rest) {
		f = strings.Trim(f, `"`)
		if k, v, ok := strings.Cut(f, "="); ok {
			if k != "rt" && k != "request_time" {
				continue
			}
			f = strings.Trim(v, `"`)
		}
		if v, err := strconv.ParseFloat(f, 64); err == nil && v >= 0 {
			return v, true
		}
	}
	return 0, false
}
