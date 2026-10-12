// Package config loads the list of services to check from a JSON file.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/GonService/go-projects/status-page/internal/monitor"
)

// File is the JSON configuration.
type File struct {
	Title    string    `json:"title"`
	Services []Service `json:"services"`
}

// Service is one entry of the configuration.
type Service struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Interval       string `json:"interval"`        // e.g. "30s"; default 30s
	Timeout        string `json:"timeout"`         // default 10s
	ExpectedStatus int    `json:"expected_status"` // default: any 2xx or 3xx
}

// Load reads and validates the configuration.
func Load(path string) (File, []monitor.Target, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, nil, err
	}
	return Parse(raw)
}

// Parse validates the JSON configuration and converts it to monitor targets.
func Parse(raw []byte) (File, []monitor.Target, error) {
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if f.Title == "" {
		f.Title = "Service status"
	}
	if len(f.Services) == 0 {
		return f, nil, fmt.Errorf("add at least one service to \"services\"")
	}
	seen := map[string]bool{}
	targets := make([]monitor.Target, 0, len(f.Services))
	for i, s := range f.Services {
		if s.Name == "" {
			return f, nil, fmt.Errorf("services[%d]: name is required", i)
		}
		if seen[s.Name] {
			return f, nil, fmt.Errorf("services[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return f, nil, fmt.Errorf("service %q: url must be http(s)://host/...", s.Name)
		}
		interval, err := duration(s.Interval, 30*time.Second)
		if err != nil || interval < 5*time.Second {
			return f, nil, fmt.Errorf("service %q: interval must be a duration of at least 5s, e.g. \"30s\"", s.Name)
		}
		timeout, err := duration(s.Timeout, 10*time.Second)
		if err != nil || timeout <= 0 || timeout > interval {
			return f, nil, fmt.Errorf("service %q: timeout must be positive and not longer than the interval", s.Name)
		}
		targets = append(targets, monitor.Target{
			Name: s.Name, URL: s.URL, Interval: interval, Timeout: timeout, ExpectedStatus: s.ExpectedStatus,
		})
	}
	return f, targets, nil
}

func duration(v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	return time.ParseDuration(v)
}
