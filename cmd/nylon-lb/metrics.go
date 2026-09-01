package main

import (
	"net/http"
	"sync/atomic"

	"github.com/encodeous/nylon/internal/buildinfo"
	"github.com/encodeous/nylon/internal/promtext"
)

// Metrics is nylon-lb's process-local metric set, served as Prometheus text
// on /metrics. The zero value is ready to use; Controller and Speaker take a
// *Metrics and treat nil as "metrics disabled", so every hook nil-checks
// before recording.
type Metrics struct {
	// Gauges: current state, overwritten on every successful reconcile.
	Leader       atomic.Int64 // 1 while this replica holds the allocator Lease
	AllocatedIPs atomic.Int64 // distinct in-pool addresses claimed by Services
	Services     atomic.Int64 // type=LoadBalancer Services in the cluster
	Announces    atomic.Int64 // /32s this node currently announces

	// Counters: monotonic totals since process start.
	Allocations    atomic.Int64 // fresh ingress assignments
	Releases       atomic.Int64 // ingress clearances
	AnnounceWrites atomic.Int64 // successful announce-file writes

	// Per-kind error counters backing nylon_lb_errors_total{kind=...}.
	errBind, errWrite, errReconcile, errList, errGlob atomic.Int64

	// node labels nylon_lb_leader; fixed at construction.
	node string
}

// NewMetrics returns a metric set labeled with the given node name. All
// gauges and counters start at zero.
func NewMetrics(node string) *Metrics {
	return &Metrics{node: node}
}

// addError increments nylon_lb_errors_total for kind. The kind set is a
// closed vocabulary; unknown kinds are ignored.
func (m *Metrics) addError(kind string) {
	switch kind {
	case "bind":
		m.errBind.Add(1)
	case "write":
		m.errWrite.Add(1)
	case "reconcile":
		m.errReconcile.Add(1)
	case "list":
		m.errList.Add(1)
	case "glob":
		m.errGlob.Add(1)
	}
}

// Handler serves the metric set as Prometheus text. Atomic loads make reads
// race-free against the reconcilers' updates, so no extra locking is needed.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := promtext.New(w)
		p.Metric("nylon_lb_build_info", "Build information for this nylon-lb process.", "gauge",
			map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit}, 1)
		p.Metric("nylon_lb_leader", "1 when this replica holds the allocator Lease.", "gauge",
			map[string]string{"node": m.node}, float64(m.Leader.Load()))
		p.Metric("nylon_lb_allocated_ips", "Distinct pool addresses currently claimed by Service ingress or spec.loadBalancerIP.", "gauge",
			nil, float64(m.AllocatedIPs.Load()))
		p.Metric("nylon_lb_services", "type=LoadBalancer Services currently in the cluster.", "gauge",
			nil, float64(m.Services.Load()))
		p.Metric("nylon_lb_announces", "LoadBalancer /32s this node currently announces.", "gauge",
			nil, float64(m.Announces.Load()))
		p.Metric("nylon_lb_allocations_total", "Fresh load balancer ingress assignments since start.", "counter",
			nil, float64(m.Allocations.Load()))
		p.Metric("nylon_lb_releases_total", "Ingress clearances (Service deleted or no longer LoadBalancer) since start.", "counter",
			nil, float64(m.Releases.Load()))
		p.Metric("nylon_lb_announce_writes_total", "Successful announce-file writes since start.", "counter",
			nil, float64(m.AnnounceWrites.Load()))
		p.Metric("nylon_lb_errors_total", "Failed operations since start, by kind.", "counter",
			map[string]string{"kind": "bind"}, float64(m.errBind.Load()))
		p.Metric("nylon_lb_errors_total", "Failed operations since start, by kind.", "counter",
			map[string]string{"kind": "write"}, float64(m.errWrite.Load()))
		p.Metric("nylon_lb_errors_total", "Failed operations since start, by kind.", "counter",
			map[string]string{"kind": "reconcile"}, float64(m.errReconcile.Load()))
		p.Metric("nylon_lb_errors_total", "Failed operations since start, by kind.", "counter",
			map[string]string{"kind": "list"}, float64(m.errList.Load()))
		p.Metric("nylon_lb_errors_total", "Failed operations since start, by kind.", "counter",
			map[string]string{"kind": "glob"}, float64(m.errGlob.Load()))
	})
}
