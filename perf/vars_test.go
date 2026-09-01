package perf

import (
	"math"
	"testing"
)

func TestSnapshotNamesAndLabels(t *testing.T) {
	// seed known values into the live package counters
	SendBatchSize.Add(7)
	SendsPerSecond.Add(42)

	samples := Snapshot()
	if len(samples) != len(perfHistograms)*3+len(perfCounters) {
		t.Fatalf("Snapshot returned %d samples, want %d", len(samples), len(perfHistograms)*3+len(perfCounters))
	}

	byLabel := map[string][]Sample{}
	for _, s := range samples {
		if s.Type != "gauge" {
			t.Fatalf("sample %s has type %q, want gauge", s.Name, s.Type)
		}
		if s.Name == "" || s.Help == "" {
			t.Fatalf("sample missing name or help: %+v", s)
		}
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			t.Fatalf("sample %s has non-finite value %v", s.Name, s.Value)
		}
		byLabel[s.Name] = append(byLabel[s.Name], s)
	}

	// every histogram must expose exactly the three quantile variants
	for _, h := range perfHistograms {
		got := byLabel[h.name]
		if len(got) != 3 {
			t.Fatalf("histogram %s produced %d samples, want 3", h.name, len(got))
		}
		seen := map[string]bool{}
		for _, s := range got {
			q := s.Labels["quantile"]
			if q != "0.5" && q != "0.9" && q != "0.99" {
				t.Fatalf("histogram %s has unexpected quantile label %q", h.name, q)
			}
			seen[q] = true
		}
		if !seen["0.5"] || !seen["0.9"] || !seen["0.99"] {
			t.Fatalf("histogram %s missing quantiles: %+v", h.name, got)
		}
	}

	// every counter must appear once, unparsable-free; the seeded one must be positive
	for _, c := range perfCounters {
		got := byLabel[c.name]
		if len(got) != 1 {
			t.Fatalf("counter %s produced %d samples, want 1", c.name, len(got))
		}
		if len(got[0].Labels) != 0 {
			t.Fatalf("counter %s has labels %+v, want none", c.name, got[0].Labels)
		}
	}
	if v := byLabel["nylon_perf_sends_per_second"][0].Value; v <= 0 {
		t.Fatalf("seeded nylon_perf_sends_per_second = %v, want > 0", v)
	}
	if v := byLabel["nylon_perf_send_batch_size"]; v[0].Value <= 0 && v[1].Value <= 0 && v[2].Value <= 0 {
		t.Fatalf("seeded nylon_perf_send_batch_size quantiles all zero: %+v", v)
	}
}

func TestSnapshotMatchesPrometheusNames(t *testing.T) {
	want := []string{
		"nylon_perf_dispatch_latency_us",
		"nylon_perf_send_batch_size",
		"nylon_perf_recv_batch_size",
		"nylon_perf_sends_per_second",
		"nylon_perf_recvs_per_second",
		"nylon_perf_sent_packets_per_second",
		"nylon_perf_recv_packets_per_second",
		"nylon_perf_sent_bytes_per_second",
		"nylon_perf_recv_bytes_per_second",
	}
	for _, name := range want {
		found := false
		for _, s := range Snapshot() {
			if s.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Snapshot is missing %s", name)
		}
	}
}
