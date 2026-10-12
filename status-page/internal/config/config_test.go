package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	_, targets, err := Parse([]byte(`{"services":[{"name":"Site","url":"https://example.com"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if targets[0].Interval != 30*time.Second || targets[0].Timeout != 10*time.Second {
		t.Fatalf("unexpected defaults %+v", targets[0])
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		`{}`:                                   "at least one service",
		`{"services":[{"url":"https://a.b"}]}`: "name is required",
		`{"services":[{"name":"a","url":"ftp://a.b"}]}`:                                "url must be",
		`{"services":[{"name":"a","url":"https://a.b","interval":"1s"}]}`:              "at least 5s",
		`{"services":[{"name":"a","url":"https://a.b","timeout":"2m"}]}`:               "timeout",
		`{"services":[{"name":"a","url":"https://a"},{"name":"a","url":"https://b"}]}`: "duplicate",
		`not json`: "invalid JSON",
	}
	for input, want := range cases {
		if _, _, err := Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%s) error = %v, want it to contain %q", input, err, want)
		}
	}
}
