// Package config loads and validates the gateway configuration (JSON).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Duration is a time.Duration written as "5s" or "250ms" in JSON.
type Duration struct{ time.Duration }

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// RateLimit is a token bucket: Burst tokens, refilled at RPS per second.
type RateLimit struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst"`
}

// APIKey identifies a client. Name appears in logs and metrics, never the key.
type APIKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// HealthCheck configures active upstream checks.
type HealthCheck struct {
	Path           string   `json:"path"`
	Interval       Duration `json:"interval"`
	Timeout        Duration `json:"timeout"`
	UnhealthyAfter int      `json:"unhealthy_after"` // consecutive failures
	HealthyAfter   int      `json:"healthy_after"`   // consecutive successes
}

// Route sends requests whose path starts with Prefix to Upstreams.
type Route struct {
	Name        string      `json:"name"`
	Prefix      string      `json:"prefix"`
	StripPrefix bool        `json:"strip_prefix"`
	Upstreams   []string    `json:"upstreams"`
	Public      bool        `json:"public"` // no API key required
	RateLimit   *RateLimit  `json:"rate_limit,omitempty"`
	HealthCheck HealthCheck `json:"health_check"`

	Targets []*url.URL `json:"-"`
}

// Config is the whole file.
type Config struct {
	Listen       string    `json:"listen"`
	AdminListen  string    `json:"admin_listen"`
	TrustProxy   bool      `json:"trust_proxy"` // use X-Forwarded-For for the client IP
	APIKeys      []APIKey  `json:"api_keys"`
	RateLimit    RateLimit `json:"rate_limit"`
	Routes       []Route   `json:"routes"`
	UpstreamWait Duration  `json:"upstream_timeout"`
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse validates the configuration and fills defaults.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AdminListen == "" {
		c.AdminListen = ":9090"
	}
	if c.UpstreamWait.Duration == 0 {
		c.UpstreamWait.Duration = 30 * time.Second
	}
	if c.RateLimit.RPS == 0 {
		c.RateLimit = RateLimit{RPS: 10, Burst: 20}
	}
	if err := c.RateLimit.validate(); err != nil {
		return nil, fmt.Errorf("rate_limit: %w", err)
	}
	if len(c.Routes) == 0 {
		return nil, errors.New("at least one route is required")
	}
	keys := map[string]bool{}
	for i, k := range c.APIKeys {
		if k.Name == "" || len(k.Key) < 16 {
			return nil, fmt.Errorf("api_keys[%d]: name is required and key must have at least 16 characters", i)
		}
		if keys[k.Key] {
			return nil, fmt.Errorf("api_keys[%d]: duplicate key", i)
		}
		keys[k.Key] = true
	}
	names, prefixes := map[string]bool{}, map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Name == "" || names[r.Name] {
			return nil, fmt.Errorf("routes[%d]: name is required and must be unique", i)
		}
		names[r.Name] = true
		if !strings.HasPrefix(r.Prefix, "/") || prefixes[r.Prefix] {
			return nil, fmt.Errorf("route %s: prefix must start with / and be unique", r.Name)
		}
		prefixes[r.Prefix] = true
		if len(r.Upstreams) == 0 {
			return nil, fmt.Errorf("route %s: at least one upstream is required", r.Name)
		}
		for _, u := range r.Upstreams {
			t, err := url.Parse(u)
			if err != nil || (t.Scheme != "http" && t.Scheme != "https") || t.Host == "" {
				return nil, fmt.Errorf("route %s: invalid upstream %q", r.Name, u)
			}
			r.Targets = append(r.Targets, t)
		}
		if r.RateLimit != nil {
			if err := r.RateLimit.validate(); err != nil {
				return nil, fmt.Errorf("route %s: rate_limit: %w", r.Name, err)
			}
		}
		if !r.Public && len(c.APIKeys) == 0 {
			return nil, fmt.Errorf("route %s requires an API key but no api_keys are configured (set \"public\": true)", r.Name)
		}
		h := &r.HealthCheck
		if h.Path == "" {
			h.Path = "/healthz"
		}
		if h.Interval.Duration == 0 {
			h.Interval.Duration = 5 * time.Second
		}
		if h.Timeout.Duration == 0 {
			h.Timeout.Duration = 2 * time.Second
		}
		if h.UnhealthyAfter <= 0 {
			h.UnhealthyAfter = 2
		}
		if h.HealthyAfter <= 0 {
			h.HealthyAfter = 1
		}
	}
	return &c, nil
}

func (r RateLimit) validate() error {
	if r.RPS <= 0 || r.Burst < 1 {
		return errors.New("rps must be > 0 and burst >= 1")
	}
	return nil
}
