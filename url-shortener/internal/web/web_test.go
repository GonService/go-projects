package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GonService/go-projects/url-shortener/internal/store"
)

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, "").Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func postJSON(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestCreateAPI(t *testing.T) {
	ts, _ := newServer(t)
	res, body := postJSON(t, ts.URL+"/api/links", `{"url":"https://example.com/docs","alias":"docs"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	if body["short_url"] != ts.URL+"/docs" || body["url"] != "https://example.com/docs" {
		t.Fatalf("unexpected body: %v", body)
	}
	if res.Header.Get("Location") != "/api/links/docs" {
		t.Fatalf("location = %q", res.Header.Get("Location"))
	}

	cases := []struct {
		body string
		want int
	}{
		{`{"url":"https://example.com/x","alias":"docs"}`, http.StatusConflict},
		{`{"url":"not a url"}`, http.StatusBadRequest},
		{`{"url":"https://example.com","alias":"a b"}`, http.StatusBadRequest},
		{`{"url":"https://example.com","extra":1}`, http.StatusBadRequest},
		{`garbage`, http.StatusBadRequest},
	}
	for _, c := range cases {
		res, body := postJSON(t, ts.URL+"/api/links", c.body)
		if res.StatusCode != c.want || body["error"] == "" {
			t.Errorf("%s: status = %d, body = %v; want %d", c.body, res.StatusCode, body, c.want)
		}
	}
}

func TestRedirectCountsClicks(t *testing.T) {
	ts, st := newServer(t)
	if _, err := st.Create("https://example.com/landing", "goto"); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: noRedirect}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/goto", nil)
		req.Header.Set("Referer", "https://blog.example.net/post")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "https://example.com/landing" {
			t.Fatalf("status = %d, location = %q", res.StatusCode, res.Header.Get("Location"))
		}
	}
	res, err := client.Get(ts.URL + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing code status = %d", res.StatusCode)
	}

	res, err = http.Get(ts.URL + "/api/links/goto")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var l struct {
		Clicks    int64            `json:"clicks"`
		Referrers map[string]int64 `json:"referrers"`
	}
	_ = json.NewDecoder(res.Body).Decode(&l)
	if l.Clicks != 3 || l.Referrers["blog.example.net"] != 3 {
		t.Fatalf("stats = %+v", l)
	}

	metrics := get(t, ts.URL+"/metrics")
	for _, want := range []string{"shortener_links 1", "shortener_clicks 3", "shortener_redirects_total 3", "shortener_not_found_total 1"} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("metrics missing %q:\n%s", want, metrics)
		}
	}
}

func TestFormFlow(t *testing.T) {
	ts, _ := newServer(t)
	client := &http.Client{CheckRedirect: noRedirect}
	res, err := client.PostForm(ts.URL+"/", url.Values{"url": {"https://example.com/form"}, "alias": {"form-link"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/form-link/stats?created=1" {
		t.Fatalf("status = %d, location = %q", res.StatusCode, res.Header.Get("Location"))
	}

	page := get(t, ts.URL+"/form-link/stats?created=1")
	for _, want := range []string{"Link created", ts.URL + "/form-link", "https://example.com/form", "Clicks in the last 14 days"} {
		if !strings.Contains(page, want) {
			t.Errorf("stats page missing %q", want)
		}
	}

	// Invalid input re-renders the form with the error and keeps the values.
	res, err = client.PostForm(ts.URL+"/", url.Values{"url": {"ftp://example.com"}, "alias": {"keep-me"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "absolute http or https") || !strings.Contains(string(body), `value="keep-me"`) {
		t.Fatalf("status = %d, body = %s", res.StatusCode, body)
	}

	index := get(t, ts.URL+"/")
	if !strings.Contains(index, "/form-link") {
		t.Fatal("index does not list the new link")
	}
}

func TestEscapesUserInput(t *testing.T) {
	ts, st := newServer(t)
	if _, err := st.Create(`https://example.com/?q=<script>alert(1)</script>`, "xss"); err != nil {
		t.Fatal(err)
	}
	page := get(t, ts.URL+"/xss/stats")
	if strings.Contains(page, "<script>alert") {
		t.Fatal("unescaped script tag in page")
	}
}

func TestBaseURLAndHeaders(t *testing.T) {
	st, _ := store.Open("")
	ts := httptest.NewServer(New(st, "https://go.example.com/").Handler())
	defer ts.Close()
	res, body := postJSON(t, ts.URL+"/api/links", `{"url":"https://example.com","alias":"base"}`)
	if body["short_url"] != "https://go.example.com/base" {
		t.Fatalf("short_url = %v", body["short_url"])
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("missing security headers")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, res.StatusCode)
	}
	b, _ := io.ReadAll(res.Body)
	return string(b)
}
