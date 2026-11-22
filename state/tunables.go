package state

import "time"

// RouterTunables contains all timing and algorithm parameters for the router.
// These are set once at startup and should not be mutated after the Nylon instance starts.
type RouterTunables struct {
	HopCost               uint32 // per-hop cost in microseconds added to every neighbour link; a multi-hop path must beat a direct one by more than the measured per-hop jitter, not by loop-prevention epsilon (was 5 µs).
	LargeChangeThreshold  uint32 // 100 milliseconds change
	SeqnoRequestHopCount  uint8
	RouteUpdateDelay      time.Duration
	ProbeDelay            time.Duration
	ProbeRecoveryDelay    time.Duration
	ProbeDiscoveryDelay   time.Duration
	StarvationDelay       time.Duration
	SeqnoDedupTTL         time.Duration
	NeighbourIOFlushDelay time.Duration
	IPCDispatchTimeout    time.Duration
	SafeMTU               int

	// WindowSamples is the sliding window size
	WindowSamples     int
	OutlierPercentage float64
	// MinimumConfidenceWindow is the minimum number of samples before we lower the ping
	MinimumConfidenceWindow int

	GcDelay           time.Duration
	LinkDeadThreshold time.Duration
	RouteExpiryTime   time.Duration
	// UnknownPrefixWarnInterval limits how often checkPrefix warns about the
	// same unknown prefix; foreign gateways routinely re-advertise their
	// zone-internal prefixes (design §4.6) and the rejection is expected.
	UnknownPrefixWarnInterval time.Duration
	// NoEndpointWarnInterval limits how often ComputeRoutes warns about the
	// same endpoint-less neighbour; recompute runs on every route update, so
	// the condition is chronic rather than novel.
	NoEndpointWarnInterval time.Duration
	LinkSwitchDeadband     float64 // We will switch to a new feasible route if: metric(new) * LinkSwitchDeadband <= metric(old). The 1.10 default covers the observed 2.8-11.5% challenger spread; surviving ±15% metric jitter would need an instantaneous ratio of ~1.32, which no sane deadband delivers, hence the temporal RouteSwitchHold gate. Do not raise the margin to fix flapping. The ±500 µs noise band is carried by RouteSwitchMinImprovement, not by a wider margin.
	// RouteSwitchMinImprovement is the absolute metric gain, in microseconds,
	// a challenger must hold beyond the relative deadband before a committed
	// next hop may change; 0 disables it. Default 500 µs: the same-link
	// metric drift measured on a production anycast mesh (one gateway
	// node) reached 362-503 µs within minutes on healthy links (probe
	// success 1.0, RTT ~1.4-1.8 ms), while the 1.1 relative deadband at a
	// ~1.8 ms base admits a switch at ~180 µs — inside the noise; every
	// measured switch (deltas 213/290/388 µs) reset long-lived TCP flows to
	// the VIP 0.17-1 s later.
	RouteSwitchMinImprovement uint32

	// Loss-aware liveness: ProbeTimeout 0 keeps v0.7.1 fire-and-forget
	// probing (no arming, no accounting); demotion additionally requires
	// MinimumConfidenceWindow probe samples.
	ProbeTimeout          time.Duration
	LossPenaltyGain       float64
	LossDemotionThreshold float64

	// Handshake staleness: Warn/Demote 0 disables that stage.
	HandshakeStaleWarn         time.Duration
	HandshakeStaleDemote       time.Duration
	HandshakeStaleDebounce     time.Duration
	HandshakeStaleWarnInterval time.Duration

	// RouteSwitchHold is the sustained-superiority window a challenger must
	// span before a metric-based switch, and the grace window the committed
	// next hop survives a brief route-level loss (advertisement INF or gone
	// while the link stays up); 0 disables both.
	RouteSwitchHold time.Duration

	// client configuration
	ClientKeepaliveInterval time.Duration
	ClientDeadThreshold     time.Duration

	// central updates
	CentralUpdateDelay time.Duration

	// healthcheck defaults
	HealthCheckDelay       time.Duration
	HealthCheckMaxFailures int

	EndpointResolveExpiry time.Duration
	EndpointResolveDelay  time.Duration

	MaxConfigSize int64
}

// NylonOptions contains runtime flags set at startup (typically from CLI flags).
type NylonOptions struct {
	DBG_log_probe        bool
	DBG_log_wireguard    bool
	DBG_log_repo_updates bool
	DBG_log_json         bool
	DBG_debug            bool
	DBG_trace            bool
	DBG_trace_tc         bool
}

func DefaultRouterTunables() RouterTunables {
	probeDelay := time.Millisecond * 1000
	routeUpdateDelay := time.Second * 5
	// Keep the hold-down on the same order as LinkDeadThreshold (5*ProbeDelay):
	// long enough to swallow a metric spike, short enough not to delay real
	// convergence.
	holdDown := 5 * probeDelay
	return RouterTunables{
		HopCost:               200,
		LargeChangeThreshold:  100 * 1000,
		SeqnoRequestHopCount:  64,
		RouteUpdateDelay:      routeUpdateDelay,
		ProbeDelay:            probeDelay,
		ProbeRecoveryDelay:    time.Millisecond * 1500,
		ProbeDiscoveryDelay:   time.Second * 10,
		StarvationDelay:       time.Millisecond * 100,
		SeqnoDedupTTL:         time.Second * 3,
		NeighbourIOFlushDelay: time.Millisecond * 500,
		IPCDispatchTimeout:    time.Second,
		SafeMTU:               1200,

		WindowSamples:           int((time.Second * 60) / probeDelay),
		OutlierPercentage:       0.05,
		MinimumConfidenceWindow: int(time.Second * 15 / probeDelay),

		GcDelay:                   time.Millisecond * 1000,
		LinkDeadThreshold:         5 * probeDelay,
		RouteExpiryTime:           5 * routeUpdateDelay,
		UnknownPrefixWarnInterval: time.Hour,
		NoEndpointWarnInterval:    time.Hour,
		LinkSwitchDeadband:        1.1,
		RouteSwitchMinImprovement: 500,
		RouteSwitchHold:           holdDown,

		ProbeTimeout:          2 * time.Second,
		LossPenaltyGain:       0.3,
		LossDemotionThreshold: 0.4,

		HandshakeStaleWarn:         135 * time.Second,
		HandshakeStaleDemote:       150 * time.Second,
		HandshakeStaleDebounce:     15 * time.Second,
		HandshakeStaleWarnInterval: time.Hour,

		ClientKeepaliveInterval: 3 * probeDelay,
		ClientDeadThreshold:     6 * probeDelay, // 2 * ClientKeepaliveInterval

		CentralUpdateDelay: time.Second * 10,

		HealthCheckDelay:       time.Second * 15,
		HealthCheckMaxFailures: 3,

		EndpointResolveExpiry: time.Minute * 1,
		EndpointResolveDelay:  time.Second * 15,

		MaxConfigSize: 1 << 20, // 1 MB
	}
}

func DefaultNylonOptions() NylonOptions {
	return NylonOptions{}
}

func ApplyTunablesOverrides(t *RouterTunables, o *TunablesOverrides) {
	if o == nil {
		return
	}
	if o.ProbeDelay != nil {
		t.ProbeDelay = *o.ProbeDelay
	}
	if o.ProbeRecoveryDelay != nil {
		t.ProbeRecoveryDelay = *o.ProbeRecoveryDelay
	}
	if o.ProbeDiscoveryDelay != nil {
		t.ProbeDiscoveryDelay = *o.ProbeDiscoveryDelay
	}
	if o.GcDelay != nil {
		t.GcDelay = *o.GcDelay
	}
	if o.LinkDeadThreshold != nil {
		t.LinkDeadThreshold = *o.LinkDeadThreshold
	}
	if o.ProbeTimeout != nil {
		t.ProbeTimeout = *o.ProbeTimeout
	}
	if o.RouteExpiryTime != nil {
		t.RouteExpiryTime = *o.RouteExpiryTime
	}
	if o.RouteSwitchHold != nil {
		t.RouteSwitchHold = *o.RouteSwitchHold
	}
	if o.NoEndpointWarnInterval != nil {
		t.NoEndpointWarnInterval = *o.NoEndpointWarnInterval
	}
	if o.HandshakeStaleWarn != nil {
		t.HandshakeStaleWarn = *o.HandshakeStaleWarn
	}
	if o.HandshakeStaleDemote != nil {
		t.HandshakeStaleDemote = *o.HandshakeStaleDemote
	}
	if o.HandshakeStaleDebounce != nil {
		t.HandshakeStaleDebounce = *o.HandshakeStaleDebounce
	}
	if o.HandshakeStaleWarnInterval != nil {
		t.HandshakeStaleWarnInterval = *o.HandshakeStaleWarnInterval
	}
	if o.LossPenaltyGain != nil {
		t.LossPenaltyGain = *o.LossPenaltyGain
	}
	if o.LossDemotionThreshold != nil {
		t.LossDemotionThreshold = *o.LossDemotionThreshold
	}
	if o.LinkSwitchDeadband != nil {
		t.LinkSwitchDeadband = *o.LinkSwitchDeadband
	}
	if o.RouteSwitchMinImprovement != nil {
		t.RouteSwitchMinImprovement = *o.RouteSwitchMinImprovement
	}
	if o.HopCost != nil {
		t.HopCost = *o.HopCost
	}
}
