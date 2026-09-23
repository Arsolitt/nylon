package perf

import (
	"encoding/json"
	"expvar"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/encodeous/metric"
)

var (
	DispatchLatency     = metric.NewHistogram("1m1s")
	SendBatchSize       = metric.NewHistogram("10s1s")
	RecvBatchSize       = metric.NewHistogram("10s1s")
	TunReadBatchSize    = metric.NewHistogram("10s1s")
	TunWriteBatchSize   = metric.NewHistogram("10s1s")
	SendsPerSecond      = metric.NewCounter("10s1s")
	RecvsPerSecond      = metric.NewCounter("10s1s")
	SentPacketPerSecond = metric.NewCounter("10s1s")
	RecvPacketPerSecond = metric.NewCounter("10s1s")
	SentBytesPerSecond  = metric.NewCounter("10s1s")
	RecvBytesPerSecond  = metric.NewCounter("10s1s")
)

// Monotonic totals of packets lost inside the TUN datapath. They are exported
// as counters, unlike the windowed rates above.
var (
	TunStagedDropsTotal     atomic.Uint64 // reader -> per-peer staged handoff, drop-oldest evictions (packets)
	TunWriteQueueDropsTotal atomic.Uint64 // bounce -> TUN writer handoff, queue full (packets)
	TunKernelTxDroppedTotal atomic.Uint64 // /sys/class/net/<iface>/statistics/tx_dropped, last sample
)

// Sample is a single Prometheus sample derived from the live perf counters.
type Sample struct {
	Name   string
	Help   string
	Type   string // gauge for windowed values, counter for monotonic totals
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
	{"nylon_perf_tun_read_batch_size", "Packets per TUN read call.", TunReadBatchSize},
	{"nylon_perf_tun_write_batch_size", "Packets per TUN write call.", TunWriteBatchSize},
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

// totalVar exposes a monotonic atomic total on the metrics page. The metric
// handler only renders expvar values that implement metric.Metric, so a plain
// expvar.Func would not show up there.
type totalVar struct{ v *atomic.Uint64 }

func (t totalVar) Add(n float64) { t.v.Add(uint64(n)) }

func (t totalVar) String() string { return strconv.FormatUint(t.v.Load(), 10) }

func (t totalVar) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"c","count":` + strconv.FormatUint(t.v.Load(), 10) + `}`), nil
}

var perfTotals = []struct {
	name string
	help string
	v    *atomic.Uint64
}{
	{"nylon_tun_staged_drops_total", "Packets evicted from a per-peer staged queue because they were too old.", &TunStagedDropsTotal},
	{"nylon_tun_write_queue_drops_total", "Bounce packets dropped because the target TUN writer queue was full.", &TunWriteQueueDropsTotal},
	{"nylon_tun_kernel_tx_dropped_total", "Kernel tx_dropped counter of the TUN interface (last sample, resets with the interface).", &TunKernelTxDroppedTotal},
}

// Snapshot converts the live counters via String() into Prometheus samples.
func Snapshot() []Sample {
	samples := make([]Sample, 0, len(perfHistograms)*3+len(perfCounters)+len(perfTotals))
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
	for _, t := range perfTotals {
		samples = append(samples, Sample{
			Name:  t.name,
			Help:  t.help,
			Type:  "counter",
			Value: float64(t.v.Load()),
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

	expvar.Publish("tun:readBatchSize", TunReadBatchSize)
	expvar.Publish("tun:writeBatchSize", TunWriteBatchSize)
	expvar.Publish("tun:stagedDrops", totalVar{&TunStagedDropsTotal})
	expvar.Publish("tun:writeQueueDrops", totalVar{&TunWriteQueueDropsTotal})
	expvar.Publish("tun:kernelTxDropped", totalVar{&TunKernelTxDroppedTotal})
}
