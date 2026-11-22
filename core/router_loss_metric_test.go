//go:build router_test

package core

import (
	"testing"
	"time"

	"github.com/encodeous/nylon/log"
	"github.com/encodeous/nylon/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lossLink attaches a real, confident NylonEndpoint to neighbour id: a stable
// rtt and lostProbes consecutive timed-out probes. MockEndpoint bypasses
// NylonEndpoint.Metric, so the loss factor reaches the router only this way.
func lossLink(t *testing.T, rs *state.RouterState, id state.NodeId, rtt time.Duration, lostProbes int) *state.NylonEndpoint {
	t.Helper()
	ep := state.NewEndpoint("127.0.0.1:0", false, nil, rs.RouterTunables)
	ep.Renew()
	for range rs.RouterTunables.MinimumConfidenceWindow {
		ep.UpdatePing(rtt)
		ep.RecordProbe(true)
	}
	for range lostProbes {
		ep.RecordProbe(false)
	}
	rs.GetNeighbour(id).Eps = append(rs.GetNeighbour(id).Eps, ep)
	return ep
}

// A lossy incumbent is displaced by a clean path only when the clean path is
// within the deadband of the penalised metric: six timed-out probes (4.9%
// sustained loss, factor 1.515) lose to an equal-latency clean path under a
// 1.5 deadband, and keep a clean path 40% slower.
func TestRouter_LossyPathLosesToCleanPath(t *testing.T) {
	hold := 5 * time.Second
	for name, tc := range map[string]struct {
		challengerRTT time.Duration
		wantSwitch    bool
	}{
		"equal-latency clean path": {10 * time.Millisecond, true},
		"clean path 40% slower":    {14 * time.Millisecond, false},
	} {
		t.Run(name, func(t *testing.T) {
			rs, h, pPrefix := stickinessSetup(hold, "B", "C")
			lossy := lossLink(t, rs, "B", 10*time.Millisecond, 6)
			clean := lossLink(t, rs, "C", tc.challengerRTT, 0)
			require.Equal(t, uint32(15149), lossy.Metric())
			require.Equal(t, uint32(tc.challengerRTT.Microseconds()), clean.Metric())
			rs.RouterTunables.LinkSwitchDeadband = 1.5
			rs.RouterTunables.RouteSwitchMinImprovement = 500

			h.NeighUpdate(rs, "B", "B", pPrefix, 0, 0)
			ComputeRoutes(rs, h)
			require.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
			h.GetActions()
			h.GetTableActions()
			rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

			h.NeighUpdate(rs, "C", "C", pPrefix, 0, 0)
			ComputeRoutes(rs, h)
			_, tracked := rs.RouteChallenger[pPrefix]
			if !tc.wantSwitch {
				assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
				assert.False(t, tracked, "a clean path outside the deadband must not open a streak")
				for range 10 {
					ComputeRoutes(rs, h)
				}
				assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
				assert.Equal(t, uint64(0), rs.RouteSwitchCounts[pPrefix])
				assert.Empty(t, switchRecords(h), "no switch means no decision record")
				return
			}
			require.True(t, tracked, "a deadband pass must open the challenger streak")
			assert.Equal(t, state.NodeId("C"), rs.RouteChallenger[pPrefix].Nh)
			rs.RouteChallenger[pPrefix] = state.RouteChallenger{Nh: "C", Since: time.Now().Add(-2 * hold)}
			ComputeRoutes(rs, h)
			require.Equal(t, "C", string(rs.Routes[pPrefix].Nh), "a sustained loss penalty must move the next hop")
			records := switchRecords(h)
			require.Len(t, records, 1)
			assert.Equal(t, []any{
				log.EventRouteSwitched, "switched next hop",
				"prefix", pPrefix,
				"old_nh", "B", "old_metric", uint64(15149),
				"new_nh", "C", "new_metric", uint64(10000),
				"delta_abs", int64(5149), "delta_ratio", 10000.0 / 15149.0,
				"hops_old", 1, "hops_new", 1,
				"held", true, "reason", "better_metric",
			}, records[0].Args)
			assert.Equal(t, uint64(1), rs.RouteSwitchCounts[pPrefix])
			stats := rs.RouteSwitchStats[pPrefix]
			assert.Equal(t, uint64(1), stats.DeltaCount)
			assert.Equal(t, int64(5149), stats.DeltaSum)
			assert.Equal(t, [len(state.RouteDeltaEdges) + 1]uint64{15: 1}, stats.DeltaBins,
				"5149 µs belongs to the overflow bucket past the edge 5000")
		})
	}
}

// Isolated timeouts are not loss: one or two timed-out probes (0.83% / 1.66%
// sustained loss, factor 1.084 / 1.169) keep a 10 ms incumbent against a clean
// path 10% faster under the 1.5 deadband (11687 × 1.5 > 9000).
func TestRouter_SmallLossNoSwitch(t *testing.T) {
	for name, lostProbes := range map[string]int{"one timed-out probe": 1, "two timed-out probes": 2} {
		t.Run(name, func(t *testing.T) {
			rs, h, pPrefix := stickinessSetup(5*time.Second, "B", "C")
			incumbent := lossLink(t, rs, "B", 10*time.Millisecond, lostProbes)
			lossLink(t, rs, "C", 9*time.Millisecond, 0)
			require.Equal(t, []uint32{10840, 11687}[lostProbes-1], incumbent.Metric())
			rs.RouterTunables.LinkSwitchDeadband = 1.5
			rs.RouterTunables.RouteSwitchMinImprovement = 500

			h.NeighUpdate(rs, "B", "B", pPrefix, 0, 0)
			ComputeRoutes(rs, h)
			require.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
			h.GetActions()
			h.GetTableActions()
			rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

			h.NeighUpdate(rs, "C", "C", pPrefix, 0, 0)
			ComputeRoutes(rs, h)
			_, tracked := rs.RouteChallenger[pPrefix]
			assert.False(t, tracked, "the deadband rejects the challenger at admission")
			for range 10 {
				ComputeRoutes(rs, h)
			}
			assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
				"sustained recomputes far beyond the hold must not move the next hop")
			assert.Equal(t, uint64(0), rs.RouteSwitchCounts[pPrefix], "no switch means no counter")
			assert.Empty(t, switchRecords(h), "no switch means no decision record")
		})
	}
}
