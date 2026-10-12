package logparse

import (
	"errors"
	"testing"
	"time"
)

func TestParseCombinedWithRequestTime(t *testing.T) {
	line := `203.0.113.9 - alice [12/Oct/2026:14:02:11 -0300] "GET /api/orders/42?expand=items HTTP/1.1" 200 512 "https://shop.example.com/" "Mozilla/5.0 (X11; Linux x86_64)" 0.123`
	e, err := Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 12, 14, 2, 11, 0, time.FixedZone("", -3*3600))
	if !e.Time.Equal(want) {
		t.Errorf("time = %v, want %v", e.Time, want)
	}
	if e.IP != "203.0.113.9" || e.Method != "GET" || e.Path != "/api/orders/42" || e.Protocol != "HTTP/1.1" {
		t.Errorf("request fields: %+v", e)
	}
	if e.Status != 200 || e.Bytes != 512 || e.Referer != "https://shop.example.com/" || e.UserAgent != "Mozilla/5.0 (X11; Linux x86_64)" {
		t.Errorf("response fields: %+v", e)
	}
	if !e.HasTime || e.RequestTime != 0.123 {
		t.Errorf("request time = %v %v", e.RequestTime, e.HasTime)
	}
}

func TestParseVariants(t *testing.T) {
	cases := []struct {
		name, line string
		path       string
		bytes      int64
		rt         float64
		hasRT      bool
	}{
		{"plain combined", `::1 - - [01/Jan/2026:00:00:00 +0000] "HEAD / HTTP/1.0" 304 0 "-" "-"`, "/", 0, 0, false},
		{"bytes dash", `10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "GET /x HTTP/1.1" 204 - "-" "curl/8"`, "/x", 0, 0, false},
		{"rt key", `10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "POST /login HTTP/2.0" 401 20 "-" "curl/8" rt=0.250 uct="0.001" urt="0.249"`, "/login", 20, 0.25, true},
		{"upstream first", `10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "GET /a HTTP/1.1" 200 1 "-" "ua" "10.1.1.1:80" 1.5`, "/a", 1, 1.5, true},
		{"escaped quote in ua", `10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "GET /q HTTP/1.1" 200 1 "-" "a \"b\" c" 0.010`, "/q", 1, 0.01, true},
		{"tls garbage", `10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "\x16\x03\x01" 400 157 "-" "-"`, "(invalid request)", 157, 0, false},
	}
	for _, c := range cases {
		e, err := Parse(c.line)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if e.Path != c.path || e.Bytes != c.bytes || e.RequestTime != c.rt || e.HasTime != c.hasRT {
			t.Errorf("%s: got path=%q bytes=%d rt=%v has=%v", c.name, e.Path, e.Bytes, e.RequestTime, e.HasTime)
		}
	}
}

func TestParseMalformed(t *testing.T) {
	bad := []string{
		"",
		"garbage",
		`10.0.0.1 - - [not a date] "GET / HTTP/1.1" 200 1 "-" "-"`,
		`10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "GET / HTTP/1.1" abc 1 "-" "-"`,
		`10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 1 "-`,
		`10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "GET / HTTP/1.1`,
		`10.0.0.1 - - [01/Jan/2026:00:00:00 +0000] "GET / HTTP/1.1" 999 1 "-" "-"`,
	}
	for _, l := range bad {
		if _, err := Parse(l); !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q) = %v, want ErrMalformed", l, err)
		}
	}
}

func BenchmarkParse(b *testing.B) {
	line := `203.0.113.9 - - [12/Oct/2026:14:02:11 -0300] "GET /api/orders/42?expand=items HTTP/1.1" 200 512 "https://shop.example.com/" "Mozilla/5.0 (X11; Linux x86_64)" 0.123`
	for i := 0; i < b.N; i++ {
		_, _ = Parse(line)
	}
}
