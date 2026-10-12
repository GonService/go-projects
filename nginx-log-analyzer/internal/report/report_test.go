package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GonService/go-projects/nginx-log-analyzer/internal/analyze"
	"github.com/GonService/go-projects/nginx-log-analyzer/internal/logparse"
)

func sample() analyze.Report {
	a := analyze.New(analyze.Options{Top: 5, Normalize: true, SpikeMin: 3, SpikeX: 3})
	base := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	for m := 0; m < 10; m++ {
		for i := 0; i < 5; i++ {
			a.Add(logparse.Entry{IP: "10.0.0.1", Time: base.Add(time.Duration(m) * time.Minute), Method: "GET", Path: "/api/items/1", Status: 200, Bytes: 2048, UserAgent: `<script>x</script>`, RequestTime: 0.05, HasTime: true})
		}
	}
	for i := 0; i < 6; i++ {
		a.Add(logparse.Entry{IP: "10.0.0.9", Time: base.Add(5 * time.Minute), Method: "POST", Path: "/pay", Status: 502, RequestTime: 3, HasTime: true})
	}
	return a.Report([]string{"access.log"})
}

func TestTable(t *testing.T) {
	var b bytes.Buffer
	if err := Table(&b, sample()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"Requests      56", "/api/items/:id", "SLOWEST ENDPOINTS", "POST /pay", "3.00 s", "ERROR SPIKES", "14:05", "5xx"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

func TestJSON(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, sample()); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["requests"].(float64) != 56 || len(got["spikes"].([]any)) != 1 || len(got["requests_per_minute"].([]any)) != 10 {
		t.Errorf("unexpected JSON: %s", b.String())
	}
}

func TestHTMLIsSelfContainedAndEscaped(t *testing.T) {
	var b bytes.Buffer
	if err := HTML(&b, sample()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "<script>x") {
		t.Error("user agent not escaped")
	}
	for _, bad := range []string{"<script src", "<link ", "http://", "https://"} {
		if strings.Contains(out, bad) {
			t.Errorf("report references external resource %q", bad)
		}
	}
	if strings.Count(out, "<rect") != 30 { // 10 bars x 3 segments
		t.Errorf("chart has %d rects", strings.Count(out, "<rect"))
	}
}

func TestChartDownsamples(t *testing.T) {
	mins := make([]analyze.Minute, 600)
	for i := range mins {
		mins[i] = analyze.Minute{Time: time.Unix(int64(i)*60, 0), Total: 10, Err5xx: 1}
	}
	c := buildChart(mins, 900, 220, 120)
	if len(c.Bars) != 120 || c.Bucket != "5 minutes" {
		t.Fatalf("bars = %d bucket = %q", len(c.Bars), c.Bucket)
	}
	if c.YLabels[4].Text != "60" { // 50 per bucket rounded up to a multiple of 4
		t.Errorf("top label = %q", c.YLabels[4].Text)
	}
}

func TestFormatting(t *testing.T) {
	if Num(1234567) != "1,234,567" || Num(int64(-1000)) != "-1,000" || Num(12) != "12" {
		t.Error("Num")
	}
	if Duration(0.0421) != "42 ms" || Duration(1.5) != "1.50 s" {
		t.Error("Duration")
	}
	if Bytes(512) != "512 B" || Bytes(1536) != "1.5 KiB" || Bytes(3<<20) != "3.0 MiB" {
		t.Error("Bytes")
	}
	for n, want := range map[int]int{1: 4, 5: 8, 37: 40, 50: 60, 82: 100, 512: 600, 1000: 1000} {
		if got := niceCeil(n); got != want {
			t.Errorf("niceCeil(%d) = %d, want %d", n, got, want)
		}
	}
}
