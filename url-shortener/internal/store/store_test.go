package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	good := []string{"https://example.com", "http://example.com/a?b=c#d", "  https://sub.example.com:8443/x  "}
	for _, u := range good {
		if _, err := ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want ok", u, err)
		}
	}
	bad := []string{"", "example.com", "ftp://example.com", "javascript:alert(1)", "https://", "/relative", "https://user:pass@example.com", "https://example.com/" + string(make([]byte, 3000))}
	for _, u := range bad {
		if _, err := ValidateURL(u); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("ValidateURL(%q) = %v, want ErrInvalidURL", u, err)
		}
	}
}

func TestValidAlias(t *testing.T) {
	cases := map[string]bool{"docs": true, "my_link-2": true, "ab": false, "has space": false, "metrics": false, "API": false, "ação": false}
	for alias, want := range cases {
		if got := ValidAlias(alias); got != want {
			t.Errorf("ValidAlias(%q) = %v, want %v", alias, got, want)
		}
	}
}

func TestCreateAndAlias(t *testing.T) {
	s, _ := Open("")
	l, err := s.Create("https://example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Code) != codeLength {
		t.Fatalf("code %q has length %d", l.Code, len(l.Code))
	}
	if _, err := s.Create("https://example.com/docs", "docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("https://example.com/other", "docs"); !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("duplicate alias: got %v", err)
	}
	if _, err := s.Create("https://example.com", "x"); !errors.Is(err, ErrInvalidAlias) {
		t.Fatalf("short alias: got %v", err)
	}
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: got %v", err)
	}
}

func TestHitCountsClicks(t *testing.T) {
	s, _ := Open("")
	day := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return day }
	if _, err := s.Create("https://example.com", "home"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Hit("home", "https://news.example.org/post"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Hit("home", ""); err != nil {
		t.Fatal(err)
	}
	l, _ := s.Get("home")
	if l.Clicks != 4 || l.Daily["2026-03-10"] != 4 || l.Referrers["news.example.org"] != 3 {
		t.Fatalf("unexpected stats: %+v", l)
	}
	if _, err := s.Hit("nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hit missing: %v", err)
	}

	// Old days fall out of the window.
	s.now = func() time.Time { return day.AddDate(0, 0, dailyWindow+1) }
	_, _ = s.Hit("home", "")
	l, _ = s.Get("home")
	if _, ok := l.Daily["2026-03-10"]; ok {
		t.Fatalf("old day not pruned: %v", l.Daily)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s, _ := Open("")
	_, _ = s.Create("https://example.com", "copy")
	_, _ = s.Hit("copy", "https://ref.example")
	l, _ := s.Get("copy")
	l.Referrers["ref.example"] = 99
	l2, _ := s.Get("copy")
	if l2.Referrers["ref.example"] != 1 {
		t.Fatal("caller modified internal state")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "links.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("https://example.com/a", "alpha"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Hit("alpha", "")
	_, _ = s.Hit("alpha", "")

	// Create is persisted immediately, clicks only after Flush.
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := reopened.Get("alpha"); l.Clicks != 0 {
		t.Fatalf("clicks before flush = %d", l.Clicks)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, _ = Open(path)
	if l, _ := reopened.Get("alpha"); l.Clicks != 2 || l.URL != "https://example.com/a" {
		t.Fatalf("after flush: %+v", l)
	}

	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestOpenRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected error for corrupt file")
	}
}

func TestRunFlushesOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	s, _ := Open(path)
	_, _ = s.Create("https://example.com", "run")
	_, _ = s.Hit("run", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, time.Hour, func(err error) { t.Error(err) }); close(done) }()
	cancel()
	<-done
	reopened, _ := Open(path)
	if l, _ := reopened.Get("run"); l.Clicks != 1 {
		t.Fatalf("clicks after shutdown flush = %d", l.Clicks)
	}
}

func TestConcurrentUse(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "links.json"))
	_, _ = s.Create("https://example.com", "hot")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.Hit("hot", "")
			}
			_, _ = s.Create("https://example.com/x", "")
			_ = s.Flush()
			_ = s.List()
		}()
	}
	wg.Wait()
	if l, _ := s.Get("hot"); l.Clicks != 1000 {
		t.Fatalf("clicks = %d, want 1000", l.Clicks)
	}
	if n, _ := s.Totals(); n != 21 {
		t.Fatalf("links = %d, want 21", n)
	}
}
