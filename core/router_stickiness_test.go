//go:build router_test

package core

import (
	"math"
	"net/netip"
	"testing"
	"time"

	"github.com/encodeous/nylon/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestRouter_SwitchesOnLargeGain: a challenger that is genuinely better
// takes over once its superiority streak and the commit-age hold have both
// spanned RouteSwitchHold — the first deadband pass only opens the streak.
func TestRouter_SwitchesOnLargeGain(t *testing.T) {
	hold := 5 * time.Second
	rs, h, pPrefix := stickinessSetup(hold, "B", "C")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))

	h.NeighUpdate(rs, "B", "B", pPrefix, 0, 1577)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
	h.GetActions()
	h.GetTableActions()
	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

	h.NeighUpdate(rs, "C", "C", pPrefix, 0, 1300)
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
		"a deadband pass must open the challenger streak, not switch at once")

	rs.RouteChallenger[pPrefix] = state.RouteChallenger{Nh: "C", Since: time.Now().Add(-2 * hold)}
	ComputeRoutes(rs, h)
	assert.Equal(t, "C", string(rs.Routes[pPrefix].Nh),
		"a sustained superior challenger must take over")
}

// TestRouter_NextHopSurvivesMetricJitter: with three gateways at the
// production metric spread, ±15% advertisement jitter makes the best
// challenger cross the deadband on isolated recomputes. The committed next
// hop must survive every one of them: a metric-based switch needs the
// superior challenger to stay eligible for a full streak window, and a
// streak interrupted for a single recompute resets.
func TestRouter_NextHopSurvivesMetricJitter(t *testing.T) {
	hold := 5 * time.Second
	rs, h, pPrefix := stickinessSetup(hold, "B", "C", "D")
	_ = AddLink(rs, NewMockEndpoint("B", 1))
	_ = AddLink(rs, NewMockEndpoint("C", 1))
	_ = AddLink(rs, NewMockEndpoint("D", 1))

	base := map[state.NodeId]uint32{"B": 1577, "C": 1621, "D": 1758}
	phase := map[state.NodeId]float64{"B": 0.0, "C": 2.1, "D": 4.2}
	jitter := func(id state.NodeId, i int) uint32 {
		s := math.Sin(float64(i)*1.7 + phase[id])
		return uint32(math.Round(float64(base[id]) * (1 + 0.15*s)))
	}

	// The whole schedule is a pure function of i, so passes and checkpoint
	// picks are deterministic.
	type tick struct {
		pass   bool
		bestNh state.NodeId
	}
	schedule := make([]tick, 51)
	for i := 1; i <= 50; i++ {
		incumbentTotal := uint64(1) + uint64(jitter("B", i))
		bestTotal, bestNh := uint64(0), state.NodeId("")
		for _, id := range []state.NodeId{"C", "D"} {
			total := uint64(1) + uint64(jitter(id, i))
			if bestNh == "" || total < bestTotal || (total == bestTotal && id < bestNh) {
				bestTotal, bestNh = total, id
			}
		}
		schedule[i] = tick{pass: float64(bestTotal)*1.1 <= float64(incumbentTotal), bestNh: bestNh}
	}
	var passIs []int
	for i := 1; i <= 50; i++ {
		if schedule[i].pass {
			passIs = append(passIs, i)
		}
	}
	require.GreaterOrEqual(t, len(passIs), 5,
		"the sequence must drive the best challenger past the deadband at least 5 times")

	// Three deadband passes double as streak-interruption checkpoints: after
	// the pass recompute, age the recorded streak, then let the next update
	// settle every advertisement back inside the deadband. The interruption
	// must reset the streak; a surviving aged streak would switch the next
	// hop on the pass after that.
	checkpoint := map[int]bool{}
	for _, idx := range []int{0, len(passIs) / 2, len(passIs) - 1} {
		checkpoint[passIs[idx]] = true
	}

	// Advertisements advance their seqno with every metric change so each
	// update stays feasible (Babel sources re-advertise with a fresh seqno).
	seq := uint16(1)
	h.NeighUpdate(rs, "B", "B", pPrefix, seq, base["B"])
	ComputeRoutes(rs, h)
	assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh))
	h.GetActions()
	h.GetTableActions()
	rs.RouteSelectedAt[pPrefix] = time.Now().Add(-time.Hour)

	settle := func(i int) {
		seq++
		h.NeighUpdate(rs, "B", "B", pPrefix, seq, jitter("B", i))
		h.NeighUpdate(rs, "C", "C", pPrefix, seq, jitter("C", i))
		h.NeighUpdate(rs, "D", "D", pPrefix, seq, jitter("D", i))
		ComputeRoutes(rs, h)
	}
	interrupt := func() {
		seq++
		h.NeighUpdate(rs, "B", "B", pPrefix, seq, base["B"])
		h.NeighUpdate(rs, "C", "C", pPrefix, seq, base["C"])
		h.NeighUpdate(rs, "D", "D", pPrefix, seq, base["D"])
		ComputeRoutes(rs, h)
	}

	for i := 1; i <= 50; i++ {
		settle(i)
		assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
			"update %d: single-recompute jitter must not move the committed next hop", i)
		if checkpoint[i] {
			rs.RouteChallenger[pPrefix] = state.RouteChallenger{
				Nh:    schedule[i].bestNh,
				Since: time.Now().Add(-2 * hold),
			}
			interrupt()
			assert.Empty(t, rs.RouteChallenger[pPrefix],
				"update %d: an interrupted streak must reset", i)
			assert.Equal(t, "B", string(rs.Routes[pPrefix].Nh),
				"update %d: the interruption settle must not move the committed next hop", i)
		}
	}
	assert.Equal(t, uint32(1+jitter("B", 50)), rs.Routes[pPrefix].Metric,
		"the incumbent metric tracks the latest advertisement through the whole run")
}
