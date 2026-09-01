package perf

import (
	"encoding/json"
	"expvar"
	"net/http"
	"strconv"

	"github.com/encodeous/metric"
)

var (
	DispatchLatency     = metric.NewHistogram("1m1s")
	SendBatchSize       = metric.NewHistogram("10s1s")
	RecvBatchSize       = metric.NewHistogram("10s1s")
	SendsPerSecond      = metric.NewCounter("10s1s")
	RecvsPerSecond      = metric.NewCounter("10s1s")
	SentPacketPerSecond = metric.NewCounter("10s1s")
	RecvPacketPerSecond = metric.NewCounter("10s1s")
	SentBytesPerSecond  = metric.NewCounter("10s1s")
	RecvBytesPerSecond  = metric.NewCounter("10s1s")
)

// Sample is a single Prometheus sample derived from the live perf counters.
type Sample struct {
	Name   string
	Help   string
	Type   string // always "gauge": windowed rates are NOT monotonic
	Labels map[string]string
	Value  float64
}

type quantiles struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

var perfHistograms = []struct {
	name string
	help string
	m    metric.Metric
}{
	{"nylon_perf_dispatch_latency_us", "Windowed main-loop dispatch latency in microseconds.", DispatchLatency},
	{"nylon_perf_send_batch_size", "Windowed number of packets per send batch.", SendBatchSize},
	{"nylon_perf_recv_batch_size", "Windowed number of packets per receive batch.", RecvBatchSize},
}

var perfCounters = []struct {
	name string
	help string
	m    metric.Metric
}{
	{"nylon_perf_sends_per_second", "WireGuard sends per second (10s windowed).", SendsPerSecond},
	{"nylon_perf_recvs_per_second", "WireGuard receives per second (10s windowed).", RecvsPerSecond},
	{"nylon_perf_sent_packets_per_second", "Packets sent per second (10s windowed).", SentPacketPerSecond},
	{"nylon_perf_recv_packets_per_second", "Packets received per second (10s windowed).", RecvPacketPerSecond},
	{"nylon_perf_sent_bytes_per_second", "Bytes sent per second (10s windowed).", SentBytesPerSecond},
	{"nylon_perf_recv_bytes_per_second", "Bytes received per second (10s windowed).", RecvBytesPerSecond},
}

// Snapshot converts the live counters via String() into Prometheus samples.
func Snapshot() []Sample {
	samples := make([]Sample, 0, len(perfHistograms)*3+len(perfCounters))
	for _, h := range perfHistograms {
		var q quantiles
		if err := json.Unmarshal([]byte(h.m.String()), &q); err != nil {
			continue
		}
		for _, quant := range []struct {
			label string
			value float64
		}{
			{"0.5", q.P50},
			{"0.9", q.P90},
			{"0.99", q.P99},
		} {
			samples = append(samples, Sample{
				Name:   h.name,
				Help:   h.help,
				Type:   "gauge",
				Labels: map[string]string{"quantile": quant.label},
				Value:  quant.value,
			})
		}
	}
	for _, c := range perfCounters {
		value, err := strconv.ParseFloat(c.m.String(), 64)
		if err != nil {
			continue
		}
		samples = append(samples, Sample{
			Name:  c.name,
			Help:  c.help,
			Type:  "gauge",
			Value: value,
		})
	}
	return samples
}

func init() {
	http.Handle("/debug/metrics", metric.Handler(metric.Exposed))
	expvar.Publish("nylon:SendBatchSize", SendBatchSize)
	expvar.Publish("nylon:RecvBatchSize", RecvBatchSize)

	expvar.Publish("nylon:SentPacket/s", SentPacketPerSecond)
	expvar.Publish("nylon:RecvPacket/s", RecvPacketPerSecond)
	expvar.Publish("nylon:Sends/s", SendsPerSecond)
	expvar.Publish("nylon:Recvs/s", RecvsPerSecond)
	expvar.Publish("nylon:SentBytes/s", SentBytesPerSecond)
	expvar.Publish("nylon:RecvBytes/s", RecvBytesPerSecond)
	expvar.Publish("nylon:DispatchLatency (µs)", DispatchLatency)
}
