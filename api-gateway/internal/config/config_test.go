package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadExample(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.AdminListen != ":9090" || len(c.Routes) != 3 || len(c.APIKeys) != 2 {
		t.Fatalf("unexpected config: %+v", c)
	}
	users := c.Routes[0]
	if len(users.Targets) != 2 || users.Targets[1].Host != "users-2:8000" || users.HealthCheck.Interval.Duration != 5*time.Second {
		t.Fatalf("users route: %+v", users)
	}
	orders := c.Routes[1]
	if orders.RateLimit == nil || orders.RateLimit.RPS != 2 || orders.HealthCheck.Path != "/healthz" || orders.HealthCheck.UnhealthyAfter != 2 {
		t.Fatalf("orders defaults: %+v", orders)
	}
	if !c.Routes[2].Public || !c.Routes[2].StripPrefix {
		t.Fatalf("catalog route: %+v", c.Routes[2])
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"routes":[{"name":"a","prefix":"/","public":true,"upstreams":["http://127.0.0.1:9000"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.RateLimit.RPS != 10 || c.RateLimit.Burst != 20 || c.UpstreamWait.Duration != 30*time.Second || c.Routes[0].HealthCheck.Timeout.Duration != 2*time.Second {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	key := `"api_keys":[{"name":"k","key":"0123456789abcdef"}]`
	cases := map[string]string{
		`{}`: "at least one route",
		`{"routes":[{"name":"a","prefix":"/","public":true,"upstreams":["ftp://x"]}]}`:                                                    "invalid upstream",
		`{"routes":[{"name":"a","prefix":"api","public":true,"upstreams":["http://x"]}]}`:                                                 "prefix must start",
		`{"routes":[{"name":"a","prefix":"/","public":true,"upstreams":[]}]}`:                                                             "at least one upstream",
		`{"routes":[{"name":"a","prefix":"/","upstreams":["http://x"]}]}`:                                                                 "no api_keys",
		`{"routes":[{"name":"a","prefix":"/","public":true,"upstreams":["http://x"]}],"extra":1}`:                                         "unknown field",
		`{"api_keys":[{"name":"k","key":"short"}],"routes":[{"name":"a","prefix":"/","upstreams":["http://x"]}]}`:                         "at least 16",
		`{` + key + `,"routes":[{"name":"a","prefix":"/","upstreams":["http://x"]},{"name":"a","prefix":"/b","upstreams":["http://x"]}]}`: "unique",
		`{` + key + `,"routes":[{"name":"a","prefix":"/","upstreams":["http://x"]},{"name":"b","prefix":"/","upstreams":["http://x"]}]}`:  "unique",
		`{` + key + `,"rate_limit":{"rps":-1,"burst":1},"routes":[{"name":"a","prefix":"/","upstreams":["http://x"]}]}`:                   "rps must be",
		`{` + key + `,"routes":[{"name":"a","prefix":"/","upstreams":["http://x"],"health_check":{"interval":"soon"}}]}`:                  "invalid duration",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%s) = %v, want error containing %q", in, err, want)
		}
	}
}
