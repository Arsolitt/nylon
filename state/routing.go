package state

import (
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"time"
)

type NodeId string

// Source is a pair of a router-id and a prefix (Babel Section 2.7).
type Source struct {
	NodeId
	netip.Prefix
}

func (s Source) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("router", string(s.NodeId)),
		slog.String("prefix", s.Prefix.String()),
	)
}

func (s Source) String() string {
	return fmt.Sprintf("(router: %s, prefix: %s)", s.NodeId, s.Prefix)
}

type Advertisement struct {
	NodeId
	Expiry        time.Time
	IsPassiveHold bool
	MetricFn      func() uint32
	ExpiryFn      func()
}
type RouterState struct {
	*RouterTunables
	Id        NodeId
	SelfSeqno map[netip.Prefix]uint16
	Routes    map[netip.Prefix]SelRoute
	// RouteSelectedAt records when the committed route for a prefix was
	// installed; only routes whose next hop is a neighbour are tracked. It is
	// maintained by ComputeRoutes and read by the route-switch hold-down.
	RouteSelectedAt map[netip.Prefix]time.Time
	Sources         map[Source]FD
	Neighbours      []*Neighbour
	// Advertised is a map tracking the prefix and the time it will be advertised until
	Advertised map[netip.Prefix]Advertisement

	// noEndpointWarns tracks the last emitted no-endpoint warning per
	// neighbour; written only from the dispatch goroutine, like the rest of
	// router state.
	noEndpointWarns map[NodeId]time.Time
	// hsStaleWarns tracks the last emitted handshake-stale warning per
	// neighbour; written only from the dispatch goroutine, like the rest of
	// router state.
	hsStaleWarns map[NodeId]time.Time
}

func (s *RouterState) GetSeqno(prefix netip.Prefix) uint16 {
	seq, ok := s.SelfSeqno[prefix]
	if !ok {
		return 0
	}
	return seq
}

func (s *RouterState) SetSeqno(prefix netip.Prefix, seqno uint16) {
	s.SelfSeqno[prefix] = seqno
}

// WarnNoEndpoint reports whether the no-endpoint warning should be emitted
// for the neighbour: at most once per NoEndpointWarnInterval. ComputeRoutes
// runs on every route update, so an endpoint-less neighbour is a chronic
// condition rather than a novel one.
func (s *RouterState) WarnNoEndpoint(id NodeId) bool {
	if s.noEndpointWarns == nil {
		s.noEndpointWarns = make(map[NodeId]time.Time)
	}
	if last, ok := s.noEndpointWarns[id]; !ok || time.Since(last) >= s.NoEndpointWarnInterval {
		s.noEndpointWarns[id] = time.Now()
		return true
	}
	return false
}

// WarnHandshakeStale reports whether the handshake-stale warning should be
// emitted for the neighbour: at most once per HandshakeStaleWarnInterval.
// The GC pass re-checks every neighbour each tick, so staleness is a chronic
// condition rather than a novel one.
func (s *RouterState) WarnHandshakeStale(id NodeId) bool {
	if s.hsStaleWarns == nil {
		s.hsStaleWarns = make(map[NodeId]time.Time)
	}
	if last, ok := s.hsStaleWarns[id]; !ok || time.Since(last) >= s.HandshakeStaleWarnInterval {
		s.hsStaleWarns[id] = time.Now()
		return true
	}
	return false
}

func (s *RouterState) StringRoutes() string {
	buf := make([]string, 0)
	for prefix, route := range s.Routes {
		buf = append(buf, fmt.Sprintf("%s via %s", prefix, route))
	}
	slices.Sort(buf)
	return strings.Join(buf, "\n")
}

type Neighbour struct {
	Id     NodeId
	Routes map[netip.Prefix]NeighRoute
	Eps    []Endpoint
	// HsStale excludes the neighbour from selection while the WireGuard
	// handshake is stale; HsStaleSince is the zero time while not stale.
	HsStale      bool
	HsStaleSince time.Time
}

type FD struct {
	Seqno  uint16
	Metric uint32
}

func (fd FD) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Uint64("seqno", uint64(fd.Seqno)),
		slog.Uint64("metric", uint64(fd.Metric)),
	)
}

type PubRoute struct {
	Source
	// FD will depend on which table the route is in. In the neighbour table,
	// it represents the metric advertised by the neighbour.
	// In the selected route table, it represents the metric that
	// the route will be advertised with.
	FD
}

func (r PubRoute) String() string {
	return fmt.Sprintf("(router: %s, prefix: %s, seqno: %d, metric: %d)", r.NodeId, r.Prefix, r.Seqno, r.Metric)
}

func (r PubRoute) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("router", string(r.NodeId)),
		slog.String("prefix", r.Prefix.String()),
		slog.Uint64("seqno", uint64(r.Seqno)),
		slog.Uint64("metric", uint64(r.Metric)),
	)
}

type NeighRoute struct {
	PubRoute
	ExpireAt time.Time // when the route expires
}

type SelRoute struct {
	PubRoute
	Nh          NodeId    // next hop node
	ExpireAt    time.Time // when the route expires
	RetractedBy []NodeId
}

func (r SelRoute) String() string {
	return fmt.Sprintf("(nh: %s, router: %s, prefix: %s, seqno: %d, metric: %d)", r.Nh, r.NodeId, r.Prefix, r.Seqno, r.Metric)
}

func (r SelRoute) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("nh", r.Nh), // Use Any if Nh is an object/interface
		slog.String("router", string(r.NodeId)),
		slog.String("prefix", r.Prefix.String()),
		slog.Uint64("seqno", uint64(r.Seqno)),
		slog.Uint64("metric", uint64(r.Metric)),
	)
}

func (s *RouterState) GetNeighbour(node NodeId) *Neighbour {
	nIdx := slices.IndexFunc(s.Neighbours, func(neighbour *Neighbour) bool {
		return neighbour.Id == node
	})
	if nIdx == -1 {
		return nil
	}
	return s.Neighbours[nIdx]
}
