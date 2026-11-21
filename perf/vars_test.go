package perf

import (
	"encoding/json"
	"expvar"
	"math"
	"testing"
)

func TestSnapshotNamesAndLabels(t *testing.T) {
	// seed known values into the live package counters
	SendBatchSize.Add(7)
	SendsPerSecond.Add(42)

	samples := Snapshot()
	if len(samples) != len(perfHistograms)*3+len(perfCounters)+len(perfTotals)+2 {
		t.Fatalf("Snapshot returned %d samples, want %d", len(samples), len(perfHistograms)*3+len(perfCounters)+len(perfTotals)+2)
	}

	totalNames := map[string]bool{}
	for _, total := range perfTotals {
		totalNames[total.name] = true
	}

	byLabel := map[string][]Sample{}
	for _, s := range samples {
		switch {
		case s.Type == "gauge":
		case s.Type == "counter" && totalNames[s.Name]:
		default:
			t.Fatalf("sample %s has type %q, want gauge or counter", s.Name, s.Type)
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
	// every monotonic total must appear once, as an unlabeled counter
	for _, total := range perfTotals {
		got := byLabel[total.name]
		if len(got) != 1 {
			t.Fatalf("total %s produced %d samples, want 1", total.name, len(got))
		}
		if got[0].Type != "counter" || len(got[0].Labels) != 0 {
			t.Fatalf("total %s has type %q and labels %+v, want an unlabeled counter", total.name, got[0].Type, got[0].Labels)
		}
	}
	if v := byLabel["nylon_perf_sends_per_second"][0].Value; math.Abs(v-4.2) > 1e-9 {
		t.Fatalf("seeded nylon_perf_sends_per_second = %v, want 4.2 (42 over the %d s window)", v, rateWindowSeconds)
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
		"nylon_perf_tun_read_batch_size",
		"nylon_perf_tun_write_batch_size",
		"nylon_perf_sends_per_second",
		"nylon_perf_recvs_per_second",
		"nylon_perf_sent_packets_per_second",
		"nylon_perf_recv_packets_per_second",
		"nylon_perf_sent_bytes_per_second",
		"nylon_perf_recv_bytes_per_second",
		"nylon_perf_rate_window_seconds",
		"nylon_tun_write_queue_capacity_frames",
		"nylon_tun_staged_drops_total",
		"nylon_tun_write_queue_drops_total",
		"nylon_tun_kernel_tx_dropped_total",
		"nylon_tun_read_frames_total",
		"nylon_tun_write_frames_total",
		"nylon_tun_write_syscalls_total",
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

func TestExpvarRateKeysAndCapacityGauge(t *testing.T) {
	const capacity = 4096
	TunWriteQueueCapacityFrames.Store(capacity)

	// The rate keys state the window whose sum they carry, so the old /s names are gone.
	for _, name := range []string{
		"nylon:Sends/10s",
		"nylon:Recvs/10s",
		"nylon:SentPacket/10s",
		"nylon:RecvPacket/10s",
		"nylon:SentBytes/10s",
		"nylon:RecvBytes/10s",
	} {
		if expvar.Get(name) == nil {
			t.Fatalf("expvar %s is not published", name)
		}
	}
	for _, name := range []string{
		"nylon:Sends/s",
		"nylon:Recvs/s",
		"nylon:SentPacket/s",
		"nylon:RecvPacket/s",
		"nylon:SentBytes/s",
		"nylon:RecvBytes/s",
	} {
		if expvar.Get(name) != nil {
			t.Fatalf("expvar %s is still published", name)
		}
	}

	raw, err := json.Marshal(expvar.Get("tun:writeQueueCapacity"))
	if err != nil {
		t.Fatalf("marshalling tun:writeQueueCapacity: %v", err)
	}
	var gauge struct {
		Type  string  `json:"type"`
		Value float64 `json:"value"`
		Mean  float64 `json:"mean"`
		Min   float64 `json:"min"`
		Max   float64 `json:"max"`
	}
	if err := json.Unmarshal(raw, &gauge); err != nil {
		t.Fatalf("unmarshalling tun:writeQueueCapacity: %v", err)
	}
	if gauge.Type != "g" {
		t.Fatalf("tun:writeQueueCapacity rendered as type %q, want a gauge", gauge.Type)
	}
	if gauge.Value != capacity || gauge.Mean != capacity || gauge.Min != capacity || gauge.Max != capacity {
		t.Fatalf("tun:writeQueueCapacity = %+v, want value=mean=min=max=%d", gauge, capacity)
	}
}
