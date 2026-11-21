//go:build router_test

package core

import (
	"net/netip"
	"testing"
	"time"

	"github.com/encodeous/nylon/state"
	"github.com/stretchr/testify/assert"
)

// Route-stickiness scenarios for committed next hops, reproduced from the
// production anycast-VIP flap. Metric spread mirrors production:
//
//	B advertises P at 1577 (incumbent gateway, total 1578)
//	C advertises P at 1621 (challenger, total 1622, ~3% worse)
//	D advertises P at 1758 (challenger, total 1759, ~11% worse)
//
// The challengers never meet the 1.1 deadband while B is feasible, so any
// committed next-hop change in these tests is a bypass of ShouldSwitch.

func stickinessSetup(hold time.Duration, ids ...state.NodeId) (*state.RouterState, *RouterHarness, netip.Prefix) {
	tunables := ConfigureConstants()
	tunables.RouteSwitchHold = hold
	h := &RouterHarness{}
	pPrefix := nodeToPrefix("P")
	rs := &state.RouterState{
		RouterTunables: tunables,
		Id:             "A",
		SelfSeqno:      make(map[netip.Prefix]uint16),
		Routes:         make(map[netip.Prefix]state.SelRoute),
		Sources:        make(map[state.Source]state.FD),
		Neighbours:     MakeNeighbours(ids...),
		Advertised:     map[netip.Prefix]state.Advertisement{nodeToPrefix("A"): {NodeId: state.NodeId("A"), Expiry: maxTime}},
	}
	return rs, h, pPrefix
}

// TestRouter_NextHopSurvivesBriefInf: the incumbent's advertisement goes INF
// for a single recompute (peer retraction or GC expiry) while its link stays
// up. The committed next hop must survive the blip without FIB churn and
// without letting the ~3% worse challenger park on the prefix.
func TestRouter_NextHopSurvivesBriefInf(t *testing.T) {
	rs, h, pPrefix := stickinessSetup(5*time.Second, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
	h.GetActions()
	h.GetTableActions()

	h.NeighUpdate(rs, "C", "C", pPrefix, 0, 1621)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"a challenger ~3% worse must not meet the deadband")

	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, state.INF)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"a route-level INF blip shorter than the hold must not move the committed next hop")
	assert.Empty(t, h.GetTableActions(), "the grace seed must not touch the FIB")

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"the returning incumbent must keep the route")
	assert.Empty(t, rs.RouteLostAt, "a recovered incumbent must clear its loss timestamp")
}

// TestRouter_FailsOverWhenPathDies: when the incumbent's advertisement stays
// INF past the hold, the committed next hop must fail over to the challenger;
// the returning incumbent then gets ordinary margin treatment and must not
// reclaim the prefix.
func TestRouter_FailsOverWhenPathDies(t *testing.T) {
	rs, h, pPrefix := stickinessSetup(5*time.Second, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	h.NeighUpdate(rs, "C", "C", pPrefix, 0, 1621)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
	h.GetActions()
	h.GetTableActions()

	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)
	h.NeighUpdate(rs, "B", "B", pPrefix, 0, state.INF)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"a route-level INF blip shorter than the hold must not move the committed next hop")

	rs.RouteLostAt[pPrefix] = time.Now().Add(-10 * time.Second)
	ComputeRoutes(rs, h)
	assert.Equal(t, "C", string(rs.Routes[pPrefix].Nh),
		"a sustained loss of the incumbent route must still fail over")

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	assert.Equal(t, "C", string(rs.Routes[pPrefix].Nh),
		"the returning incumbent gets ordinary margin treatment; the deadband protects the new incumbent")
}
