package log

const (
	// Scopes
	ScopeRouter        = "router"
	ScopePolyamide     = "polyamide"
	ScopeIPC           = "ipc"
	ScopePrefixes      = "dynamic-prefixes"
	ScopeHealth        = "prefix-health"
	ScopeObservability = "observability"
	ScopeAllocator     = "allocator"
	ScopeSpeaker       = "speaker"
)

const (
	// Router Events
	EventRouteInserted           = "route_inserted"
	EventRouteUpdated            = "route_updated"
	EventRouteRetracted          = "route_retracted"
	EventRouteExpired            = "route_expired"
	EventMajorRouteChange        = "major_route_change"
	EventInconsistentState       = "inconsistent_state"
	EventNoEndpointToNeigh       = "no_endpoint_to_neighbour"
	EventHandshakeStale          = "handshake_stale"
	EventHandshakeStaleDemoted   = "handshake_stale_demoted"
	EventHandshakeStaleRecovered = "handshake_stale_recovered"
	EventSeqnoRequested          = "seqno_requested"
)
