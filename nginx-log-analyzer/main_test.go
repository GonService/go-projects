package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSampleLog(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"-format", "json", "testdata/access.log"}, nil, &out, &errOut); err != nil {
		t.Fatal(err, errOut.String())
	}
	var r struct {
		Requests  int `json:"requests"`
		Malformed int `json:"malformed_lines"`
		Spikes    []struct {
			Class string `json:"class"`
		} `json:"spikes"`
		HasTimes bool `json:"has_request_time"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Requests < 1000 || r.Malformed != 1 || !r.HasTimes {
		t.Fatalf("unexpected report: %+v", r)
	}
	classes := map[string]int{}
	for _, s := range r.Spikes {
		classes[s.Class]++
	}
	if classes["4xx"] == 0 || classes["5xx"] == 0 {
		t.Fatalf("sample log should show 4xx and 5xx spikes, got %+v", r.Spikes)
	}
}

func TestGzipStdinAndFiles(t *testing.T) {
	plain, err := os.ReadFile("testdata/access.log")
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(plain)
	_ = zw.Close()

	var fromStdin, fromFile bytes.Buffer
	if err := run([]string{"-format", "table"}, bytes.NewReader(gz.Bytes()), &fromStdin, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	gzPath := filepath.Join(t.TempDir(), "access.log.1.gz")
	if err := os.WriteFile(gzPath, gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{gzPath}, nil, &fromFile, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	first := func(s string) string { return strings.SplitN(s, "Unique IPs", 2)[0] }
	stdinReq := strings.Split(first(fromStdin.String()), "Requests")[1]
	fileReq := strings.Split(first(fromFile.String()), "Requests")[1]
	if stdinReq != fileReq {
		t.Fatalf("stdin and file reports differ: %q vs %q", stdinReq, fileReq)
	}

	// Two files are merged into one report.
	var both bytes.Buffer
	if err := run([]string{"-format", "json", "testdata/access.log", gzPath}, nil, &both, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var r struct{ Requests int }
	_ = json.Unmarshal(both.Bytes(), &r)
	var one struct{ Requests int }
	var single bytes.Buffer
	_ = run([]string{"-format", "json", "testdata/access.log"}, nil, &single, &bytes.Buffer{})
	_ = json.Unmarshal(single.Bytes(), &one)
	if r.Requests != 2*one.Requests {
		t.Fatalf("merged requests = %d, want %d", r.Requests, 2*one.Requests)
	}
}

func TestHTMLToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.html")
	var errOut bytes.Buffer
	if err := run([]string{"-format", "html", "-o", path, "testdata/access.log"}, nil, &bytes.Buffer{}, &errOut); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(b, []byte("Nginx access log report")) {
		t.Fatalf("report not written: %v", err)
	}
	if !strings.Contains(errOut.String(), "wrote") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestBadInput(t *testing.T) {
	if err := run([]string{"-format", "xml"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("unknown format accepted")
	}
	if err := run([]string{"does-not-exist.log"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("missing file accepted")
	}
	var out bytes.Buffer
	if err := run(nil, strings.NewReader("not a log\n"), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "malformed lines skipped: 1") {
		t.Errorf("empty report:\n%s", out.String())
	}
}
