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

// Decision records for committed routes, on the route-stickiness scenarios
// of TestRouter_* (same metric spread, link cost 1, HopCost 0).

// switchRecords returns the route_switched decisions the harness has recorded
// since its actions were last drained.
func switchRecords(h *RouterHarness) []RouterEvent {
	var records []RouterEvent
	for _, action := range h.actions {
		if action.Type != eventRouterLog || len(action.Args) == 0 {
			continue
		}
		if event, ok := action.Args[0].(string); ok && event == log.EventRouteSwitched {
			records = append(records, action)
		}
	}
	return records
}

// TestRouterSwitchRecord_BetterMetric: a challenger that clears the
// sustained-gain and commit-age gate yields exactly one decision record with
// both committed metrics and the delta between them; a recompute that changes
// nothing yields none.
func TestRouterSwitchRecord_BetterMetric(t *testing.T) {
	hold := 5 * time.Second
	rs, h, pPrefix := stickinessSetup(hold, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	h.GetActions()
	h.GetTableActions()
	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

	h.NeighUpdate(rs, "C", "C", pPrefix, 0, 1300)
	ComputeRoutes(rs, h)
	assert.Empty(t, switchRecords(h), "a deadband pass must only open the challenger streak")

	rs.RouteChallenger[pPrefix] = state.RouteChallenger{Nh: "C", Since: time.Now().Add(-2 * hold)}
	ComputeRoutes(rs, h)
	require.Equal(t, "C", string(rs.Routes[pPrefix].Nh))
	records := switchRecords(h)
	require.Len(t, records, 1)
	assert.Equal(t, []any{
		log.EventRouteSwitched, "switched next hop",
		"prefix", pPrefix,
		"old_nh", "B", "old_metric", uint64(1578),
		"new_nh", "C", "new_metric", uint64(1301),
		"delta_abs", int64(277), "delta_ratio", 1301.0 / 1578.0,
		"hops_old", 1, "hops_new", 1,
		"held", true, "reason", "better_metric",
	}, records[0].Args)

	ComputeRoutes(rs, h)
	assert.Len(t, switchRecords(h), 1, "an unchanged route table must not yield a record")

	stats := rs.RouteSwitchStats[pPrefix]
	assert.Equal(t, map[state.SwitchReason]uint64{state.SwitchReasonFirstRoute: 1, state.SwitchReasonBetterMetric: 1}, stats.Reasons)
	assert.Equal(t, uint64(1), stats.DeltaCount)
	assert.Equal(t, int64(277), stats.DeltaSum)
	assert.Equal(t, [len(state.RouteDeltaEdges) + 1]uint64{11: 1}, stats.DeltaBins,
		"277 µs belongs to the bucket bounded by the edge 500")
}

// TestRouterSwitchRecord_FailoverAfterLoss: when the incumbent's
// advertisement stays lost past the grace, the failover record carries the
// stale metric the incumbent was seeded with rather than INF, so the takeover
// shows as the loss it is; the returning incumbent adds no record.
func TestRouterSwitchRecord_FailoverAfterLoss(t *testing.T) {
	rs, h, pPrefix := stickinessSetup(5*time.Second, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	h.NeighUpdate(rs, "C", "C", pPrefix, 0, 1621)
	ComputeRoutes(rs, h)
	h.GetActions()
	h.GetTableActions()
	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, state.INF)
	ComputeRoutes(rs, h)
	assert.Empty(t, switchRecords(h), "the incumbent must survive the blip inside the grace")

	rs.RouteLostAt[pPrefix] = time.Now().Add(-10 * time.Second)
	ComputeRoutes(rs, h)
	require.Equal(t, "C", string(rs.Routes[pPrefix].Nh))
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

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	assert.Len(t, switchRecords(h), 1, "the returning incumbent must not yield a record")
	assert.Equal(t, uint64(1), rs.RouteSwitchCounts[pPrefix])

	stats := rs.RouteSwitchStats[pPrefix]
	assert.Equal(t, map[state.SwitchReason]uint64{state.SwitchReasonFirstRoute: 1, state.SwitchReasonReinsertAfterLoss: 1}, stats.Reasons)
	assert.Equal(t, uint64(1), stats.DeltaCount)
	assert.Equal(t, int64(-44), stats.DeltaSum)
	assert.Equal(t, [len(state.RouteDeltaEdges) + 1]uint64{7: 1}, stats.DeltaBins,
		"-44 µs belongs to the bucket bounded by the edge 0")
}

// TestRouterSwitchRecord_FirstRoute: the first neighbour route committed for
// a prefix is recorded once, with empty old fields and no delta. It is not a
// next-hop change: the switch counter stays at zero.
func TestRouterSwitchRecord_FirstRoute(t *testing.T) {
	rs, h, pPrefix := stickinessSetup(0, "B")
	_ = AddLink(rs, NewMockEndpoint("B", 1))

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	records := switchRecords(h)
	require.Len(t, records, 1)
	assert.Equal(t, []any{
		log.EventRouteSwitched, "selected first route",
		"prefix", pPrefix,
		"old_nh", "", "old_metric", uint64(0),
		"new_nh", "B", "new_metric", uint64(1578),
		"delta_abs", int64(0), "delta_ratio", float64(0),
		"hops_old", 0, "hops_new", 1,
		"held", false, "reason", "first_route",
	}, records[0].Args)
	assert.Equal(t, uint64(0), rs.RouteSwitchCounts[pPrefix])

	ComputeRoutes(rs, h)
	assert.Len(t, switchRecords(h), 1, "an unchanged route table must not yield a record")

	stats := rs.RouteSwitchStats[pPrefix]
	assert.Equal(t, map[state.SwitchReason]uint64{state.SwitchReasonFirstRoute: 1}, stats.Reasons)
	assert.Equal(t, uint64(0), stats.DeltaCount)
	assert.Equal(t, [len(state.RouteDeltaEdges) + 1]uint64{}, stats.DeltaBins)
}

// TestRouterSwitchRecord_OriginChange: when the origin of a prefix moves to
// another router, the committed route loses its identity and the better
// advertisement of the new origin takes over at once, without any gate. The
// record names that bypass, and both paths are transit: the protocol carries
// no hop depth, so their hops are 0.
func TestRouterSwitchRecord_OriginChange(t *testing.T) {
	rs, h, pPrefix := stickinessSetup(5*time.Second, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))

	h.NeighUpdate(rs, "B", "X", pPrefix, 0, 1577)
	h.NeighUpdate(rs, "C", "X", pPrefix, 0, 1621)
	ComputeRoutes(rs, h)
	require.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
	h.GetActions()
	h.GetTableActions()

	h.NeighUpdate(rs, "B", "Y", pPrefix, 0, 1621)
	h.NeighUpdate(rs, "C", "Y", pPrefix, 0, 1300)
	ComputeRoutes(rs, h)
	require.Equal(t, "C", string(rs.Routes[pPrefix].Nh))
	records := switchRecords(h)
	require.Len(t, records, 1)
	assert.Equal(t, []any{
		log.EventRouteSwitched, "switched next hop",
		"prefix", pPrefix,
		"old_nh", "B", "old_metric", uint64(1578),
		"new_nh", "C", "new_metric", uint64(1301),
		"delta_abs", int64(277), "delta_ratio", 1301.0 / 1578.0,
		"hops_old", 0, "hops_new", 0,
		"held", false, "reason", "seqno_change",
	}, records[0].Args)

	stats := rs.RouteSwitchStats[pPrefix]
	assert.Equal(t, map[state.SwitchReason]uint64{state.SwitchReasonFirstRoute: 1, state.SwitchReasonSeqnoChange: 1}, stats.Reasons)
	assert.Equal(t, uint64(1), stats.DeltaCount)
	assert.Equal(t, int64(277), stats.DeltaSum)
	assert.Equal(t, [len(state.RouteDeltaEdges) + 1]uint64{11: 1}, stats.DeltaBins,
		"277 µs belongs to the bucket bounded by the edge 500")
}
