package state

import (
	"testing"
	"time"
)

func TestGetNeighbour(t *testing.T) {
	n1 := &Neighbour{Id: "node1"}
	n2 := &Neighbour{Id: "node2"}
	rs := &RouterState{
		Neighbours: []*Neighbour{n1, n2},
	}

	if got := rs.GetNeighbour("node1"); got != n1 {
		t.Errorf("Expected neighbour node1 to be %+v, got %+v", n1, got)
	}
	if got := rs.GetNeighbour("node2"); got != n2 {
		t.Errorf("Expected neighbour node2 to be %+v, got %+v", n2, got)
	}
	if got := rs.GetNeighbour("node3"); got != nil {
		t.Errorf("Expected nil for missing neighbour node3, got %+v", got)
	}
}

func TestNoEndpointWarnRateLimit(t *testing.T) {
	tunables := DefaultRouterTunables()
	rs := &RouterState{RouterTunables: &tunables}

	if !rs.WarnNoEndpoint("a") {
		t.Error("first warning for a neighbour must be emitted")
	}
	if rs.WarnNoEndpoint("a") {
		t.Error("immediate re-warn for the same neighbour must be suppressed")
	}
	if !rs.WarnNoEndpoint("b") {
		t.Error("a different neighbour has its own budget")
	}

	rs.noEndpointWarns["a"] = time.Now().Add(-2 * rs.NoEndpointWarnInterval)
	if !rs.WarnNoEndpoint("a") {
		t.Error("warning must re-arm once the interval elapses")
	}
	if rs.WarnNoEndpoint("b") {
		t.Error("an un-aged neighbour must stay suppressed")
	}
}

func TestWarnHandshakeStale(t *testing.T) {
	tunables := DefaultRouterTunables()
	rs := &RouterState{RouterTunables: &tunables}

	if !rs.WarnHandshakeStale("a") {
		t.Error("first warning for a neighbour must be emitted")
	}
	if rs.WarnHandshakeStale("a") {
		t.Error("immediate re-warn for the same neighbour must be suppressed")
	}
	if !rs.WarnHandshakeStale("b") {
		t.Error("a different neighbour has its own budget")
	}

	rs.hsStaleWarns["a"] = time.Now().Add(-2 * rs.HandshakeStaleWarnInterval)
	if !rs.WarnHandshakeStale("a") {
		t.Error("warning must re-arm once the interval elapses")
	}
	if rs.WarnHandshakeStale("b") {
		t.Error("an un-aged neighbour must stay suppressed")
	}
}
