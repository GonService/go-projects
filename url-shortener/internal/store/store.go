// Package store keeps short links in memory and persists them to a JSON file.
//
// Links are written to disk as soon as they are created. Click counters change
// on every redirect, so they are flushed in the background (and on shutdown)
// instead of rewriting the file per request. Every write goes to a temporary
// file in the same directory that is then renamed over the old one, so a crash
// never leaves a half-written file behind.
package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors returned by Create and Get.
var (
	ErrNotFound     = errors.New("link not found")
	ErrAliasTaken   = errors.New("alias already in use")
	ErrInvalidAlias = errors.New("alias must be 3-32 characters: letters, digits, '-' or '_'")
	ErrInvalidURL   = errors.New("url must be an absolute http or https address")
)

const (
	codeLength   = 7
	maxURLLength = 2048
	dailyWindow  = 30 // days of per-day click history kept per link
	alphabet     = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// reserved codes would shadow application routes.
var reserved = map[string]bool{"api": true, "metrics": true, "healthz": true, "static": true, "links": true}

// Link is a short code pointing to a target URL plus its click statistics.
type Link struct {
	Code      string           `json:"code"`
	URL       string           `json:"url"`
	CreatedAt time.Time        `json:"created_at"`
	Clicks    int64            `json:"clicks"`
	LastClick *time.Time       `json:"last_click,omitempty"`
	Daily     map[string]int64 `json:"daily,omitempty"`     // clicks per day, YYYY-MM-DD (UTC)
	Referrers map[string]int64 `json:"referrers,omitempty"` // clicks per referrer host
}

// Store is safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	path  string
	links map[string]*Link
	dirty bool
	now   func() time.Time
}

// Open loads links from path. A missing file starts an empty store; an empty
// path keeps everything in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, links: map[string]*Link{}, now: time.Now}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Link
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, l := range list {
		s.links[l.Code] = l
	}
	return s, nil
}

// ValidateURL checks that raw is an absolute http(s) URL with a host.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLength {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", ErrInvalidURL
	}
	return u.String(), nil
}

// ValidAlias reports whether alias can be used as a custom code.
func ValidAlias(alias string) bool {
	return aliasPattern.MatchString(alias) && !reserved[strings.ToLower(alias)]
}

// Create stores a new link. An empty alias generates a random code.
func (s *Store) Create(rawURL, alias string) (Link, error) {
	target, err := ValidateURL(rawURL)
	if err != nil {
		return Link{}, err
	}
	alias = strings.TrimSpace(alias)
	if alias != "" && !ValidAlias(alias) {
		return Link{}, ErrInvalidAlias
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	code := alias
	if code == "" {
		for {
			if code, err = randomCode(); err != nil {
				return Link{}, err
			}
			if _, taken := s.links[code]; !taken {
				break
			}
		}
	} else if _, taken := s.links[code]; taken {
		return Link{}, ErrAliasTaken
	}

	l := &Link{Code: code, URL: target, CreatedAt: s.now().UTC()}
	s.links[code] = l
	if err := s.saveLocked(); err != nil {
		delete(s.links, code)
		return Link{}, err
	}
	return copyLink(l), nil
}

// Get returns a copy of the link.
func (s *Store) Get(code string) (Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[code]
	if !ok {
		return Link{}, ErrNotFound
	}
	return copyLink(l), nil
}

// Hit records a click and returns the target URL. referrer may be empty.
func (s *Store) Hit(code, referrer string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[code]
	if !ok {
		return "", ErrNotFound
	}
	now := s.now().UTC()
	l.Clicks++
	l.LastClick = &now
	if l.Daily == nil {
		l.Daily = map[string]int64{}
	}
	l.Daily[now.Format(time.DateOnly)]++
	cutoff := now.AddDate(0, 0, -dailyWindow).Format(time.DateOnly)
	for day := range l.Daily {
		if day < cutoff {
			delete(l.Daily, day)
		}
	}
	if host := referrerHost(referrer); host != "" {
		if l.Referrers == nil {
			l.Referrers = map[string]int64{}
		}
		l.Referrers[host]++
	}
	s.dirty = true
	return l.URL, nil
}

// List returns copies of all links, newest first.
func (s *Store) List() []Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Link, 0, len(s.links))
	for _, l := range s.links {
		out = append(out, copyLink(l))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Code < out[j].Code
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Totals returns the number of links and the sum of clicks.
func (s *Store) Totals() (links int, clicks int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		clicks += l.Clicks
	}
	return len(s.links), clicks
}

// Flush writes pending click counters to disk.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

// Run flushes pending changes every interval until ctx is cancelled, then
// flushes one last time.
func (s *Store) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.Flush(); err != nil && onErr != nil {
				onErr(err)
			}
			return
		case <-t.C:
			if err := s.Flush(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// saveLocked writes the whole store atomically. The caller holds s.mu.
func (s *Store) saveLocked() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}
	list := make([]*Link, 0, len(s.links))
	for _, l := range s.links {
		list = append(list, l)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Code < list[j].Code })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, data); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func randomCode() (string, error) {
	b := make([]byte, codeLength)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}

func referrerHost(ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func copyLink(l *Link) Link {
	c := *l
	if l.LastClick != nil {
		t := *l.LastClick
		c.LastClick = &t
	}
	c.Daily = copyMap(l.Daily)
	c.Referrers = copyMap(l.Referrers)
	return c
}

func copyMap(m map[string]int64) map[string]int64 {
	if m == nil {
		return nil
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
