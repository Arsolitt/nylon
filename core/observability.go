package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/encodeous/nylon/internal/buildinfo"
	"github.com/encodeous/nylon/internal/promtext"
	"github.com/encodeous/nylon/log"
	"github.com/encodeous/nylon/perf"
	"github.com/encodeous/nylon/protocol"
)

const observabilityTimeout = time.Second

type observabilityServer struct {
	server    *http.Server
	listener  net.Listener
	closeOnce sync.Once
}

type discoveryGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

func (n *Nylon) startObservability() error {
	if n.LocalCfg.ObservabilityAddr == "" {
		return nil
	}
	obsLog := n.Log.With("module", log.ScopeObservability)

	listener, err := net.Listen("tcp", n.LocalCfg.ObservabilityAddr)
	if err != nil {
		return fmt.Errorf("listen on observability address %q: %w", n.LocalCfg.ObservabilityAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", n.handleHealth)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		n.handleReady(obsLog, w, r)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		n.handleMetrics(obsLog, w, r)
	})
	mux.HandleFunc("/discovery", n.handleDiscovery)

	obs := &observabilityServer{
		listener: listener,
		server: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	n.observability = obs
	obsLog.Info("observability server started", "address", listener.Addr())

	go func() {
		if err := obs.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			obsLog.Error("observability server failed", "error", err)
			n.Cancel(fmt.Errorf("observability server failed: %w", err))
		}
	}()
	go func() {
		<-n.Context.Done()
		obs.close()
	}()
	return nil
}

func (o *observabilityServer) close() {
	o.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = o.server.Shutdown(ctx)
	})
}

func (n *Nylon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if n.Context.Err() != nil {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (n *Nylon) statusSnapshot(ctx context.Context) (*protocol.StatusResponse, error) {
	result := make(chan *protocol.StatusResponse, 1)
	select {
	case n.DispatchChannel <- func() error {
		result <- handleStatus(n, &protocol.StatusRequest{}).GetStatus()
		return nil
	}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.Context.Done():
		return nil, context.Cause(n.Context)
	}

	select {
	case status := <-result:
		return status, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.Context.Done():
		return nil, context.Cause(n.Context)
	}
}

func (n *Nylon) handleReady(logger *slog.Logger, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), observabilityTimeout)
	defer cancel()
	if _, err := n.statusSnapshot(ctx); err != nil {
		// only surface state transitions, not every failed probe
		if n.readyLast.Swap(false) {
			logger.Warn("readiness failed", "error", err)
		} else {
			logger.Debug("readiness failed", "error", err)
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if !n.readyLast.Swap(true) {
		logger.Info("readiness recovered")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (n *Nylon) handleMetrics(logger *slog.Logger, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), observabilityTimeout)
	defer cancel()
	status, err := n.statusSnapshot(ctx)
	if err != nil {
		logger.Debug("metrics snapshot failed", "error", err)
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writePrometheusMetrics(w, status)
	perfMetrics := promtext.New(w)
	for _, sample := range perf.Snapshot() {
		perfMetrics.Metric(sample.Name, sample.Help, sample.Type, sample.Labels, sample.Value)
	}
	perfMetrics.Metric("nylon_build_info", "Nylon build information.", "gauge", map[string]string{
		"version": buildinfo.Version,
		"commit":  buildinfo.Commit,
	}, 1)
}

func (n *Nylon) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	_, port, err := net.SplitHostPort(n.LocalCfg.ObservabilityAddr)
	if err != nil {
		http.Error(w, "service discovery unavailable", http.StatusInternalServerError)
		return
	}
	groups := make([]discoveryGroup, 0)
	for _, node := range n.CentralCfg.GetNodes() {
		targets := make([]string, 0, len(node.Addresses))
		for _, addr := range node.Addresses {
			targets = append(targets, net.JoinHostPort(addr.String(), port))
		}
		if len(targets) != 0 {
			nodeType := "router"
			if n.CentralCfg.IsClient(node.Id) {
				nodeType = "passive"
			}
			groups = append(groups, discoveryGroup{
				Targets: targets,
				Labels: map[string]string{
					"nylon_node":      string(node.Id),
					"nylon_node_type": nodeType,
				},
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(groups)
}

func writePrometheusMetrics(w io.Writer, status *protocol.StatusResponse) {
	metrics := promtext.New(w)
	node := status.GetNode()
	stats := node.GetStats()
	metrics.Metric("nylon_up", "Whether the nylon daemon is ready.", "gauge", nil, 1)
	metrics.Metric("nylon_config_timestamp_seconds", "Unix timestamp of the active central configuration.", "gauge", nil, float64(node.ConfigTimestamp/int64(time.Second)))
	metrics.Metric("nylon_neighbours", "Number of configured neighbours.", "gauge", nil, float64(stats.NeighbourCount))
	metrics.Metric("nylon_active_endpoints", "Number of active peer endpoints.", "gauge", nil, float64(stats.ActiveEndpointCount))
	metrics.Metric("nylon_selected_routes", "Number of selected Babel routes.", "gauge", nil, float64(stats.SelectedRouteCount))
	metrics.Metric("nylon_advertised_prefixes", "Number of locally advertised prefixes.", "gauge", nil, float64(stats.AdvertisedPrefixCount))
	metrics.Metric("nylon_wireguard_transmit_bytes_total", "WireGuard bytes transmitted by this node.", "counter", nil, float64(stats.TxBytes))
	metrics.Metric("nylon_wireguard_receive_bytes_total", "WireGuard bytes received by this node.", "counter", nil, float64(stats.RxBytes))

	for _, neigh := range status.Neighbours {
		labels := map[string]string{"peer": neigh.PeerId}
		wg := neigh.GetWireguard()
		metrics.Metric("nylon_wireguard_peer_transmit_bytes_total", "WireGuard bytes transmitted to a peer.", "counter", labels, float64(wg.TxBytes))
		metrics.Metric("nylon_wireguard_peer_receive_bytes_total", "WireGuard bytes received from a peer.", "counter", labels, float64(wg.RxBytes))
		handshake := float64(0)
		if wg.LatestHandshakeUnix > 0 {
			handshake = float64(wg.LatestHandshakeUnix / int64(time.Second))
		}
		metrics.Metric("nylon_wireguard_peer_latest_handshake_seconds", "Unix time of the latest WireGuard handshake.", "gauge", labels, handshake)
		for _, endpoint := range neigh.Endpoints {
			epLabels := map[string]string{"peer": neigh.PeerId, "endpoint": endpoint.Address}
			active := float64(0)
			if endpoint.Active {
				active = 1
			}
			metrics.Metric("nylon_endpoint_active", "Whether a peer endpoint is active.", "gauge", epLabels, active)
			metrics.Metric("nylon_endpoint_metric", "Current Babel endpoint metric.", "gauge", epLabels, float64(endpoint.Metric))
			metrics.Metric("nylon_endpoint_rtt_seconds", "Filtered endpoint round-trip time.", "gauge", epLabels, float64(endpoint.FilteredRttNs)/float64(time.Second))
		}
	}
	for _, route := range status.GetRoutes().GetSelected() {
		pub := route.GetPubRoute()
		labels := map[string]string{
			"prefix":   pub.GetSource().GetPrefix(),
			"router":   pub.GetSource().GetNodeId(),
			"next_hop": route.GetNh(),
		}
		metrics.Metric("nylon_route_metric", "Metric of a selected Babel route.", "gauge", labels, float64(pub.GetFd().GetMetric()))
	}
}
