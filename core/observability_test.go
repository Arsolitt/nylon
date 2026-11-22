package core

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/encodeous/nylon/protocol"
	"github.com/encodeous/nylon/state"
	"github.com/stretchr/testify/require"
)

func TestObservabilityHealth(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	n := &Nylon{Context: ctx}

	rec := httptest.NewRecorder()
	n.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok\n", rec.Body.String())

	cancel(context.Canceled)
	rec = httptest.NewRecorder()
	n.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestObservabilityDiscovery(t *testing.T) {
	n := &Nylon{ConfigState: state.ConfigState{
		LocalCfg: state.LocalCfg{ObservabilityAddr: "0.0.0.0:9090"},
		CentralCfg: state.CentralCfg{
			Routers: []state.RouterCfg{{
				NodeCfg: state.NodeCfg{
					Id:        "alice",
					Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("fd00::1")},
				},
			}},
			Clients: []state.ClientCfg{{
				NodeCfg: state.NodeCfg{
					Id:        "phone",
					Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.2")},
				},
			}},
		},
	}}
	rec := httptest.NewRecorder()
	n.handleDiscovery(rec, httptest.NewRequest(http.MethodGet, "/discovery", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `[
		{"targets":["10.0.0.1:9090","[fd00::1]:9090"],"labels":{"nylon_node":"alice","nylon_node_type":"router"}},
		{"targets":["10.0.0.2:9090"],"labels":{"nylon_node":"phone","nylon_node_type":"passive"}}
	]`, rec.Body.String())
}

func TestPrometheusMetrics(t *testing.T) {
	status := &protocol.StatusResponse{
		Node: &protocol.NodeStatus{
			ConfigTimestamp: 1731117600 * int64(time.Second),
			Stats:           &protocol.NodeStats{TxBytes: 12},
		},
		Neighbours: []*protocol.NeighbourInfo{
			{
				PeerId: "bob",
				Wireguard: &protocol.WireGuardPeerStats{
					TxBytes:             7,
					LatestHandshakeUnix: 1786226710556904600,
				},
			},
			{PeerId: "eve", Wireguard: &protocol.WireGuardPeerStats{TxBytes: 5}},
		},
	}
	var buf bytes.Buffer
	writePrometheusMetrics(&buf, status)
	output := buf.String()
	require.Contains(t, output, "# TYPE nylon_wireguard_transmit_bytes_total counter")
	require.Contains(t, output, "nylon_config_timestamp_seconds 1731117600\n")
	require.Contains(t, output, `nylon_wireguard_peer_transmit_bytes_total{peer="bob"} 7`)
	require.Contains(t, output, `nylon_wireguard_peer_latest_handshake_seconds{peer="bob"} 1786226710`)
	require.NotContains(t, output, `nylon_wireguard_peer_latest_handshake_seconds{peer="bob"} 1.786`)
	require.Equal(t, 1, strings.Count(output, "# HELP nylon_wireguard_peer_transmit_bytes_total "))
}

// metricSamples returns the rendered sample lines of one metric, without its
// HELP and TYPE headers.
func metricSamples(output, name string) []string {
	var samples []string
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(line, name); ok && (rest == "" || rest[0] == '{' || rest[0] == ' ') {
			samples = append(samples, line)
		}
	}
	return samples
}

// TestPrometheusRouteSwitchDeltaMetrics: the decisions recorded for a
// selected route render as a cumulative histogram whose top bucket equals the
// count, plus sum, count and sorted reason counters. A route without recorded
// decisions renders none of them, and nylon_route_switch_total is unchanged.
func TestPrometheusRouteSwitchDeltaMetrics(t *testing.T) {
	status := &protocol.StatusResponse{
		Node: &protocol.NodeStatus{
			Stats: &protocol.NodeStats{},
		},
		Routes: &protocol.RouteTables{
			Selected: []*protocol.SelRoute{
				{
					PubRoute: &protocol.PubRoute{
						Source: &protocol.Source{NodeId: "a", Prefix: "10.0.0.0/24"},
						Fd:     &protocol.FD{Metric: 1578},
					},
					Nh:       "a",
					Switches: 2,
					SwitchStats: &protocol.RouteSwitchStats{
						DeltaBins:  []uint64{0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0},
						DeltaSum:   -123,
						DeltaCount: 2,
						Reasons:    map[string]uint64{"reinsert_after_loss": 1, "better_metric": 1},
					},
				},
				{
					PubRoute: &protocol.PubRoute{
						Source: &protocol.Source{NodeId: "b", Prefix: "10.0.1.0/24"},
						Fd:     &protocol.FD{Metric: 1622},
					},
					Nh: "b",
				},
			},
		},
	}
	var buf bytes.Buffer
	writePrometheusMetrics(&buf, status)
	output := buf.String()
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "nylon_route_switch") {
			t.Log(line)
		}
	}

	require.Equal(t, []string{
		`nylon_route_switch_delta_bucket{le="-5000",prefix="10.0.0.0/24"} 0`,
		`nylon_route_switch_delta_bucket{le="-2000",prefix="10.0.0.0/24"} 0`,
		`nylon_route_switch_delta_bucket{le="-1000",prefix="10.0.0.0/24"} 0`,
		`nylon_route_switch_delta_bucket{le="-500",prefix="10.0.0.0/24"} 0`,
		`nylon_route_switch_delta_bucket{le="-250",prefix="10.0.0.0/24"} 1`,
		`nylon_route_switch_delta_bucket{le="-100",prefix="10.0.0.0/24"} 1`,
		`nylon_route_switch_delta_bucket{le="-50",prefix="10.0.0.0/24"} 1`,
		`nylon_route_switch_delta_bucket{le="0",prefix="10.0.0.0/24"} 1`,
		`nylon_route_switch_delta_bucket{le="50",prefix="10.0.0.0/24"} 1`,
		`nylon_route_switch_delta_bucket{le="100",prefix="10.0.0.0/24"} 1`,
		`nylon_route_switch_delta_bucket{le="250",prefix="10.0.0.0/24"} 1`,
		`nylon_route_switch_delta_bucket{le="500",prefix="10.0.0.0/24"} 2`,
		`nylon_route_switch_delta_bucket{le="1000",prefix="10.0.0.0/24"} 2`,
		`nylon_route_switch_delta_bucket{le="2000",prefix="10.0.0.0/24"} 2`,
		`nylon_route_switch_delta_bucket{le="5000",prefix="10.0.0.0/24"} 2`,
		`nylon_route_switch_delta_bucket{le="+Inf",prefix="10.0.0.0/24"} 2`,
	}, metricSamples(output, "nylon_route_switch_delta_bucket"))
	require.Equal(t, []string{`nylon_route_switch_delta_sum{prefix="10.0.0.0/24"} -123`},
		metricSamples(output, "nylon_route_switch_delta_sum"))
	require.Equal(t, []string{`nylon_route_switch_delta_count{prefix="10.0.0.0/24"} 2`},
		metricSamples(output, "nylon_route_switch_delta_count"))
	require.Equal(t, []string{
		`nylon_route_switch_reasons_total{prefix="10.0.0.0/24",reason="better_metric"} 1`,
		`nylon_route_switch_reasons_total{prefix="10.0.0.0/24",reason="reinsert_after_loss"} 1`,
	}, metricSamples(output, "nylon_route_switch_reasons_total"))
	require.Equal(t, []string{
		`nylon_route_switch_total{prefix="10.0.0.0/24"} 2`,
		`nylon_route_switch_total{prefix="10.0.1.0/24"} 0`,
	}, metricSamples(output, "nylon_route_switch_total"))
}
