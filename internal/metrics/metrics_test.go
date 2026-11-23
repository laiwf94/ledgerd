package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestExposition(t *testing.T) {
	r := New()
	c := r.Counter("reqs_total", `code="200"`, "Requests.")
	c.Inc()
	c.Add(2)
	r.Counter("reqs_total", `code="500"`, "Requests.").Inc()
	if r.Counter("reqs_total", `code="200"`, "Requests.") != c {
		t.Fatal("counter not reused")
	}
	h := r.Histogram("lat_seconds", `route="/x"`, "Latency.")
	h.Observe(30 * time.Microsecond)
	h.Observe(2 * time.Millisecond)
	h.Observe(10 * time.Second)
	if r.Histogram("lat_seconds", `route="/x"`, "Latency.") != h || h.Count() != 3 {
		t.Fatal("histogram not reused")
	}
	r.Histogram("plain_seconds", "", "No labels.").Observe(time.Millisecond)
	r.Gauge("head", "Head.", func() float64 { return 12 })
	r.LabeledGauge("open", `bucket="0-2"`, "Open items.", func() float64 { return 3 })
	r.LabeledGauge("open", `bucket="30+"`, "Open items.", func() float64 { return 1 })
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE reqs_total counter",
		`reqs_total{code="200"} 3`,
		`reqs_total{code="500"} 1`,
		"# TYPE head gauge",
		"head 12",
		"# HELP open Open items.\n# TYPE open gauge\n" + `open{bucket="0-2"} 3` + "\n" + `open{bucket="30+"} 1` + "\n",
		"# TYPE lat_seconds histogram",
		`lat_seconds_bucket{route="/x",le="0.00005"} 1`,
		`lat_seconds_bucket{route="/x",le="0.0025"} 2`,
		`lat_seconds_bucket{route="/x",le="+Inf"} 3`,
		`lat_seconds_count{route="/x"} 3`,
		`plain_seconds_bucket{le="0.001"} 1`,
		"plain_seconds_count 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "# TYPE open") != 1 {
		t.Error("labelled gauge TYPE line repeated")
	}
	if strings.Count(out, "# TYPE reqs_total") != 1 {
		t.Error("TYPE line repeated")
	}
}

func TestFormatFloat(t *testing.T) {
	for in, want := range map[float64]string{0: "0", 1: "1", 0.5: "0.5", 0.00005: "0.00005", 2.5: "2.5"} {
		if got := formatFloat(in); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", in, got, want)
		}
	}
}
