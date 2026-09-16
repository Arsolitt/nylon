package core

import (
	"net/netip"
	"testing"

	"github.com/encodeous/nylon/state"
	"github.com/gaissmai/bart"
	"github.com/stretchr/testify/assert"
)

// tcTestNylon builds a Nylon with Forward/Exit tables mirroring
// TableInsertRoute semantics: every route enters Forward, self routes
// additionally enter Exit.
func tcTestNylon(id state.NodeId, routes map[netip.Prefix]RouteTableEntry) *Nylon {
	n := &Nylon{
		ConfigState: state.ConfigState{
			LocalCfg: state.LocalCfg{Id: id},
		},
	}
	forward := new(bart.Table[RouteTableEntry])
	exit := new(bart.Table[RouteTableEntry])
	for prefix, entry := range routes {
		forward.Insert(prefix, entry)
		if entry.Nh == id {
			exit.Insert(prefix, entry)
		}
	}
	n.router.Tables.Store(&ForwardingTables{Forward: forward, Exit: exit})
	return n
}

func TestTCLocalExitForwardsLearnedSpecificsInsideSelfAggregates(t *testing.T) {
	// gateway scenario in a two-zone test mesh: the node originates a zone
	// aggregate, a shared slice, a VIP and its own address, and learns
	// own-zone peer /32s plus foreign aggregates
	n := tcTestNylon("node-b", map[netip.Prefix]RouteTableEntry{
		pfx("10.100.0.0/24"):   {Nh: "node-b"}, // self zone aggregate
		pfx("10.100.0.100/32"): {Nh: "node-b"}, // self VIP
		pfx("10.100.0.2/32"):   {Nh: "node-b"}, // own address
		pfx("10.110.0.0/24"):   {Nh: "node-b"}, // self shared slice
		pfx("10.100.0.1/32"):   {Nh: "node-a"}, // learned own-zone peer
		pfx("10.100.0.3/32"):   {Nh: "node-c"}, // learned own-zone peer
		pfx("10.100.1.0/24"):   {Nh: "node-d"}, // foreign aggregate
		pfx("10.100.1.2/32"):   {Nh: "node-d"}, // foreign gateway address
	})

	// learned more-specifics inside the self aggregate must forward to their
	// nexthops, not bounce as locally destined
	for dst, wantNh := range map[string]state.NodeId{
		"10.100.0.1": "node-a",
		"10.100.0.3": "node-c",
		"10.100.1.2": "node-d",
	} {
		addr := netip.MustParseAddr(dst)
		assert.False(t, n.tcLocalExit(addr), "dst %s must not be a local exit", dst)
		entry, ok := n.router.Tables.Load().Forward.Lookup(addr)
		if assert.True(t, ok, "dst %s must have a forward route", dst) {
			assert.Equal(t, wantNh, entry.Nh, "dst %s must forward to %s", dst, wantNh)
		}
	}

	// the node's own address and VIP stay local exits
	assert.True(t, n.tcLocalExit(netip.MustParseAddr("10.100.0.2")))
	assert.True(t, n.tcLocalExit(netip.MustParseAddr("10.100.0.100")))
}

func TestTCLocalExitDoesNotBounceHeldBlackholedSpecifics(t *testing.T) {
	// a retracted /32 is held as an exact-prefix blackhole (Babel hold time);
	// the covering self aggregate must not resurrect delivery for it
	n := tcTestNylon("node-b", map[netip.Prefix]RouteTableEntry{
		pfx("10.100.0.0/24"): {Nh: "node-b"},
		pfx("10.100.0.1/32"): {Nh: "node-a", Blackhole: true},
	})

	addr := netip.MustParseAddr("10.100.0.1")
	assert.False(t, n.tcLocalExit(addr), "held blackholed dst must not bounce via the covering aggregate")
	entry, ok := n.router.Tables.Load().Forward.Lookup(addr)
	if assert.True(t, ok) {
		assert.True(t, entry.Blackhole, "the forward filter drops this dst")
	}
}

func TestTCLocalExitUncoveredSelfAggregateDstIsTerminal(t *testing.T) {
	// a dst inside a self-originated aggregate with no selected more-specific
	// is terminal on this node: it bounces to the system, which has no such
	// address configured (only /32s are on lo/nylon0) and answers
	// unreachable; it must never be forwarded into the mesh. Aggregate
	// bounces are load-bearing for pod CIDRs (bounce -> kernel -> cni0).
	n := tcTestNylon("node-b", map[netip.Prefix]RouteTableEntry{
		pfx("10.100.0.0/24"): {Nh: "node-b"},
		pfx("10.100.0.1/32"): {Nh: "node-a"},
	})

	addr := netip.MustParseAddr("10.100.0.77")
	assert.True(t, n.tcLocalExit(addr), "uncovered dst inside a self aggregate is terminal here")
	entry, ok := n.router.Tables.Load().Forward.Lookup(addr)
	if assert.True(t, ok) {
		assert.Equal(t, state.NodeId("node-b"), entry.Nh, "no mesh nexthop for an uncovered dst")
	}
}
