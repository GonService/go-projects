package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestCounterAndHistogram(t *testing.T) {
	c := NewCounter("req_total", "Requests.")
	c.Inc(Labels("route", "a", "code", "200"))
	c.Inc(Labels("route", "a", "code", "200"))
	c.Inc(Labels("route", "b", "code", "502"))
	if c.Sum("") != 3 || c.Sum(`code="5`) != 1 || c.Get(Labels("route", "a", "code", "200")) != 2 {
		t.Fatal("sums")
	}
	h := NewHistogram("dur_seconds", "Duration.", []float64{0.1, 1})
	h.Observe(Labels("route", "a"), 0.05)
	h.Observe(Labels("route", "a"), 0.5)
	h.Observe(Labels("route", "a"), 3)

	var b bytes.Buffer
	c.Write(&b)
	h.Write(&b)
	Gauge(&b, "up", "Up.", map[string]float64{Labels("target", `x"y`): 1})
	out := b.String()
	for _, want := range []string{
		"# TYPE req_total counter",
		`req_total{route="a",code="200"} 2`,
		`dur_seconds_bucket{route="a",le="0.1"} 1`,
		`dur_seconds_bucket{route="a",le="1"} 2`,
		`dur_seconds_bucket{route="a",le="+Inf"} 3`,
		`dur_seconds_sum{route="a"} 3.55`,
		`dur_seconds_count{route="a"} 3`,
		`up{target="x\"y"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
