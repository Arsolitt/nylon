package promtext

import (
	"bytes"
	"strings"
	"testing"
)

func writeSamples(w *Writer) {
	// gauges without labels
	w.Metric("nylon_up", "Whether the nylon daemon is ready.", "gauge", nil, 1)
	// histogram-style: repeated samples under one name, quantile labels,
	// exactly as the perf bridge emits them
	w.Metric("nylon_perf_dispatch_latency_us", "Windowed main-loop dispatch latency in microseconds.", "gauge",
		map[string]string{"quantile": "0.5"}, 12.5)
	w.Metric("nylon_perf_dispatch_latency_us", "Windowed main-loop dispatch latency in microseconds.", "gauge",
		map[string]string{"quantile": "0.9"}, 240)
	w.Metric("nylon_perf_dispatch_latency_us", "Windowed main-loop dispatch latency in microseconds.", "gauge",
		map[string]string{"quantile": "0.99"}, 1024.5)
	// label sorting must be deterministic regardless of map order
	w.Metric("nylon_lb_errors_total", "Failed operations since start, by kind.", "counter",
		map[string]string{"kind": "write", "node": "lb-2"}, 3)
	// values needing quoting
	w.Metric("nylon_quoted", "Labels are quoted.", "gauge",
		map[string]string{"path": `C:\path with "quotes"`}, 0)
}

func TestWriterFormat(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	writeSamples(w)
	out := buf.String()

	for _, want := range []string{
		"# HELP nylon_up Whether the nylon daemon is ready.\n# TYPE nylon_up gauge\nnylon_up 1\n",
		`nylon_perf_dispatch_latency_us{quantile="0.5"} 12.5`,
		`nylon_perf_dispatch_latency_us{quantile="0.9"} 240`,
		`nylon_perf_dispatch_latency_us{quantile="0.99"} 1024.5`,
		// sorted label keys: kind before node
		`nylon_lb_errors_total{kind="write",node="lb-2"} 3`,
		`nylon_quoted{path="C:\\path with \"quotes\""} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestHelpTypeEmittedOncePerName(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	writeSamples(w)
	out := buf.String()

	for _, name := range []string{"nylon_up", "nylon_perf_dispatch_latency_us", "nylon_lb_errors_total", "nylon_quoted"} {
		if got := strings.Count(out, "# HELP "+name+" "); got != 1 {
			t.Errorf("HELP for %s emitted %d times, want 1", name, got)
		}
		if got := strings.Count(out, "# TYPE "+name+" "); got != 1 {
			t.Errorf("TYPE for %s emitted %d times, want 1", name, got)
		}
	}
	// three quantile samples share one name
	if got := strings.Count(out, "nylon_perf_dispatch_latency_us{"); got != 3 {
		t.Errorf("quantile samples = %d, want 3", got)
	}
}

func TestValueFormatting(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	w.Metric("m_int", "i.", "gauge", nil, 3)
	w.Metric("m_frac", "f.", "gauge", nil, 0.125)
	w.Metric("m_zero", "z.", "gauge", nil, 0)
	w.Metric("m_neg", "n.", "gauge", nil, -1.5)
	out := buf.String()
	for _, want := range []string{"m_int 3\n", "m_frac 0.125\n", "m_zero 0\n", "m_neg -1.5\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}
