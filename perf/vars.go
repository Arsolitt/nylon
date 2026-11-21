package perf

import (
	"encoding/json"
	"expvar"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/encodeous/metric"
)

const (
	// rateFrame is the metric library window spec used by the rate counters:
	// 10 one-second buckets, summed by the library.
	rateFrame = "10s1s"
	// rateWindowSeconds is rateFrame's total duration. The library returns the
	// sum of that window, so dividing by it yields a per-second rate.
	rateWindowSeconds = 10
)

var (
	DispatchLatency     = metric.NewHistogram("1m1s")
	SendBatchSize       = metric.NewHistogram("10s1s")
	RecvBatchSize       = metric.NewHistogram("10s1s")
	TunReadBatchSize    = metric.NewHistogram("10s1s")
	TunWriteBatchSize   = metric.NewHistogram("10s1s")
	SendsPerSecond      = metric.NewCounter(rateFrame)
	RecvsPerSecond      = metric.NewCounter(rateFrame)
	SentPacketPerSecond = metric.NewCounter(rateFrame)
	RecvPacketPerSecond = metric.NewCounter(rateFrame)
	SentBytesPerSecond  = metric.NewCounter(rateFrame)
	RecvBytesPerSecond  = metric.NewCounter(rateFrame)
)

// Monotonic totals of packets lost inside the TUN datapath. They are exported
// as counters, unlike the windowed rates above.
var (
	TunStagedDropsTotal     atomic.Uint64 // reader -> per-peer staged handoff, drop-oldest evictions (packets)
	TunWriteQueueDropsTotal atomic.Uint64 // bounce -> TUN writer handoff, queue full (packets)
	TunKernelTxDroppedTotal atomic.Uint64 // /sys/class/net/<iface>/statistics/tx_dropped, last sample
	TunReadFramesTotal      atomic.Uint64 // frames read from the Linux TUN read path
	TunWriteFramesTotal     atomic.Uint64 // frames handed to the Linux TUN write path
	TunWriteSyscallsTotal   atomic.Uint64 // write() syscalls issued to deliver them
)

// TunWriteQueueCapacityFrames is the per-writer bounce queue budget, published
// as a gauge so the drop counters can be read against their limit. The device
// package stores it once at init.
var TunWriteQueueCapacityFrames atomic.Uint64

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
	{"nylon_perf_sends_per_second", "WireGuard sends per second, averaged over the rolling 10 s window.", SendsPerSecond},
	{"nylon_perf_recvs_per_second", "WireGuard receives per second, averaged over the rolling 10 s window.", RecvsPerSecond},
	{"nylon_perf_sent_packets_per_second", "Packets sent per second, averaged over the rolling 10 s window.", SentPacketPerSecond},
	{"nylon_perf_recv_packets_per_second", "Packets received per second, averaged over the rolling 10 s window.", RecvPacketPerSecond},
	{"nylon_perf_sent_bytes_per_second", "Bytes sent per second, averaged over the rolling 10 s window.", SentBytesPerSecond},
	{"nylon_perf_recv_bytes_per_second", "Bytes received per second, averaged over the rolling 10 s window.", RecvBytesPerSecond},
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

// gaugeVar exposes a live atomic value as a gauge on the metrics page, so it
// renders as a gauge instead of a counter. Add is a no-op: the value is owned
// by another package and only read here.
type gaugeVar struct{ v *atomic.Uint64 }

func (gaugeVar) Add(float64) {}

func (g gaugeVar) String() string { return strconv.FormatUint(g.v.Load(), 10) }

func (g gaugeVar) MarshalJSON() ([]byte, error) {
	value := strconv.FormatUint(g.v.Load(), 10)
	return []byte(`{"type":"g","value":` + value + `,"mean":` + value + `,"min":` + value + `,"max":` + value + `}`), nil
}

var perfTotals = []struct {
	name string
	help string
	v    *atomic.Uint64
}{
	{"nylon_tun_staged_drops_total", "Packets evicted from a per-peer staged queue because they were too old.", &TunStagedDropsTotal},
	{"nylon_tun_write_queue_drops_total", "Bounce packets dropped because the target TUN writer queue was full.", &TunWriteQueueDropsTotal},
	{"nylon_tun_kernel_tx_dropped_total", "Kernel tx_dropped counter of the TUN interface (last sample, resets with the interface).", &TunKernelTxDroppedTotal},
	{"nylon_tun_read_frames_total", "Frames read from the TUN device.", &TunReadFramesTotal},
	{"nylon_tun_write_frames_total", "Frames handed to the Linux TUN write path.", &TunWriteFramesTotal},
	{"nylon_tun_write_syscalls_total", "write() syscalls issued to deliver them; the frames/syscalls ratio is the offload coalescing factor.", &TunWriteSyscallsTotal},
}

// Snapshot converts the live counters via String() into Prometheus samples.
func Snapshot() []Sample {
	samples := make([]Sample, 0, len(perfHistograms)*3+len(perfCounters)+len(perfTotals)+2)
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
			Value: value / rateWindowSeconds,
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
	samples = append(samples,
		Sample{
			Name:  "nylon_perf_rate_window_seconds",
			Help:  "Averaging window in seconds behind the nylon_perf_*_per_second samples.",
			Type:  "gauge",
			Value: rateWindowSeconds,
		},
		Sample{
			Name:  "nylon_tun_write_queue_capacity_frames",
			Help:  "Frames a single TUN writer queue buffers before bounce packets are dropped.",
			Type:  "gauge",
			Value: float64(TunWriteQueueCapacityFrames.Load()),
		},
	)
	return samples
}

func init() {
	http.Handle("/debug/metrics", metric.Handler(metric.Exposed))
	expvar.Publish("nylon:SendBatchSize", SendBatchSize)
	expvar.Publish("nylon:RecvBatchSize", RecvBatchSize)

	expvar.Publish("nylon:SentPacket/10s", SentPacketPerSecond)
	expvar.Publish("nylon:RecvPacket/10s", RecvPacketPerSecond)
	expvar.Publish("nylon:Sends/10s", SendsPerSecond)
	expvar.Publish("nylon:Recvs/10s", RecvsPerSecond)
	expvar.Publish("nylon:SentBytes/10s", SentBytesPerSecond)
	expvar.Publish("nylon:RecvBytes/10s", RecvBytesPerSecond)
	expvar.Publish("nylon:DispatchLatency (µs)", DispatchLatency)

	expvar.Publish("tun:readBatchSize", TunReadBatchSize)
	expvar.Publish("tun:writeBatchSize", TunWriteBatchSize)
	expvar.Publish("tun:writeQueueCapacity", gaugeVar{&TunWriteQueueCapacityFrames})
	expvar.Publish("tun:stagedDrops", totalVar{&TunStagedDropsTotal})
	expvar.Publish("tun:writeQueueDrops", totalVar{&TunWriteQueueDropsTotal})
	expvar.Publish("tun:kernelTxDropped", totalVar{&TunKernelTxDroppedTotal})
	expvar.Publish("tun:readFrames", totalVar{&TunReadFramesTotal})
	expvar.Publish("tun:writeFrames", totalVar{&TunWriteFramesTotal})
	expvar.Publish("tun:writeSyscalls", totalVar{&TunWriteSyscallsTotal})
}
