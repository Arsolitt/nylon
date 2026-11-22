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

// Absolute-floor scenarios for ShouldSwitch's RouteSwitchMinImprovement, on
// the route-stickiness setup (link cost 1, HopCost 0, totals = adv + 1).
// The floor is set to the production default 500 µs in every test.

// TestRouter_NoSwitchBelowAbsoluteFloor: the measured same-link drift pairs
// from a production anycast mesh have ratios 0.82-0.88, so each one CLEARS
// the 1.1 relative deadband — only the absolute floor blocks the switch.
func TestRouter_NoSwitchBelowAbsoluteFloor(t *testing.T) {
	for name, tc := range map[string]struct{ old, new uint32 }{
		"drift-213us": {old: 1811, new: 1598},
		"drift-290us": {old: 2101, new: 1811},
		"drift-388us": {old: 2173, new: 1785},
	} {
		t.Run(name, func(t *testing.T) {
			rs, h, pPrefix := stickinessSetup(5*time.Second, "B", "C")
			_ = AddLink(rs, NewMockEndpoint("B", 1))
			_ = AddLink(rs, NewMockEndpoint("C", 1))
			rs.RouterTunables.RouteSwitchMinImprovement = 500

			h.NeighUpdate(rs, "B", "B", pPrefix, 0, tc.old)
			ComputeRoutes(rs, h)
			require.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
			h.GetActions()
			h.GetTableActions()

			rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)
			h.NeighUpdate(rs, "C", "C", pPrefix, 0, tc.new)
			ComputeRoutes(rs, h)
			assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
				"a challenger inside the absolute floor must not displace the committed next hop")
			_, tracked := rs.RouteChallenger[pPrefix]
			assert.False(t, tracked,
				"the floor rejects at admission, so the streak must never open")

			for range 10 {
				ComputeRoutes(rs, h)
			}
			assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
				"sustained recomputes far beyond the hold must not bypass the floor")
			assert.Equal(t, uint64(0), rs.RouteSwitchCounts[pPrefix],
				"no switch means no counter")
			assert.Empty(t, switchRecords(h), "no switch means no decision record")
		})
	}
}

// TestRouter_SwitchesAboveAbsoluteFloor: a challenger 700 µs better (deadband
// ratio 0.611, floor delta 700 ≥ 500) still has to span the streak and
// commit-age gates, then yields exactly one decision record and counter bump.
func TestRouter_SwitchesAboveAbsoluteFloor(t *testing.T) {
	hold := 5 * time.Second
	rs, h, pPrefix := stickinessSetup(hold, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))
	rs.RouterTunables.RouteSwitchMinImprovement = 500

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1800)
	ComputeRoutes(rs, h)
	require.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
	h.GetActions()
	h.GetTableActions()
	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

	h.NeighUpdate(rs, "C", "C", pPrefix, 0, 1100)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"a challenger above the floor still needs the streak and commit-age gates")
	tracked, tok := rs.RouteChallenger[pPrefix]
	if assert.True(t, tok, "a floor pass must open the challenger streak") {
		assert.Equal(t, state.NodeId("C"), tracked.Nh)
	}

	rs.RouteChallenger[pPrefix] = state.RouteChallenger{Nh: "C", Since: time.Now().Add(-2 * hold)}
	ComputeRoutes(rs, h)
	require.Equal(t, "C", string(rs.Routes[pPrefix].Nh),
		"an aged streak above the floor must take over")
	records := switchRecords(h)
	require.Len(t, records, 1)
	assert.Equal(t, []any{
		log.EventRouteSwitched, "switched next hop",
		"prefix", pPrefix,
		"old_nh", "B", "old_metric", uint64(1801),
		"new_nh", "C", "new_metric", uint64(1101),
		"delta_abs", int64(700), "delta_ratio", 1101.0 / 1801.0,
		"hops_old", 1, "hops_new", 1,
		"held", true, "reason", "better_metric",
	}, records[0].Args)
	assert.Equal(t, uint64(1), rs.RouteSwitchCounts[pPrefix])

	stats := rs.RouteSwitchStats[pPrefix]
	assert.Equal(t, map[state.SwitchReason]uint64{state.SwitchReasonFirstRoute: 1, state.SwitchReasonBetterMetric: 1}, stats.Reasons)
	assert.Equal(t, uint64(1), stats.DeltaCount)
	assert.Equal(t, int64(700), stats.DeltaSum)
	assert.Equal(t, [len(state.RouteDeltaEdges) + 1]uint64{12: 1}, stats.DeltaBins,
		"700 µs belongs to the bucket bounded by the edge 1000")
}

// TestRouter_FailsOverRegardlessOfFloor: with the floor at the production
// 500 µs and a WORSE standby (the relative deadband fails too), losing the
// incumbent must still fail over in one recompute via the loss-grace bypass.
func TestRouter_FailsOverRegardlessOfFloor(t *testing.T) {
	rs, h, pPrefix := stickinessSetup(5*time.Second, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))
	rs.RouterTunables.RouteSwitchMinImprovement = 500

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	require.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
	h.GetActions()
	h.GetTableActions()
	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

	h.NeighUpdate(rs, "C", "C", pPrefix, 0, 1621)
	ComputeRoutes(rs, h)
	require.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"the worse challenger must stay parked")

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, state.INF)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"a route-level INF blip inside the grace must not move the committed next hop")

	rs.RouteLostAt[pPrefix] = time.Now().Add(-10 * time.Second)
	ComputeRoutes(rs, h)
	require.Equal(t, "C", string(rs.Routes[pPrefix].Nh),
		"failover past the grace must happen in one recompute despite the floor")
	records := switchRecords(h)
	require.Len(t, records, 1)
	assert.Equal(t, []any{
		log.EventRouteSwitched, "switched next hop",
		"prefix", pPrefix,
		"old_nh", "B", "old_metric", uint64(1578),
		"new_nh", "C", "new_metric", uint64(1622),
		"delta_abs", int64(-44), "delta_ratio", 1622.0 / 1578.0,
		"hops_old", 1, "hops_new", 1,
		"held", true, "reason", "reinsert_after_loss",
	}, records[0].Args)
	assert.Equal(t, uint64(1), rs.RouteSwitchCounts[pPrefix])
}
