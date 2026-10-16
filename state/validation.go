package state

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"time"
)

var namePattern, _ = regexp.Compile("^[0-9a-z._/-]+$")

func NameValidator(s string) error {
	if !namePattern.MatchString(s) {
		return fmt.Errorf("%s is not a valid name, must match pattern %s", s, namePattern.String())
	}
	if len(s) > 100 {
		return fmt.Errorf("len(\"%s\") = %d > 100 is too long", s, len(s))
	}
	return nil
}

func NodeConfigValidator(central *CentralCfg, node *LocalCfg) error {
	err := NameValidator(string(node.Id))
	if err != nil {
		return err
	}
	if node.Port == 0 {
		return fmt.Errorf("port must be greater than 0")
	}
	if node.Key == [32]byte{} {
		return fmt.Errorf("private key must not be empty")
	}
	if node.NoTun && node.UseSystemRouting {
		return fmt.Errorf("no_tun cannot be used with use_system_routing")
	}
	if node.InterfaceName != "" {
		err = NameValidator(node.InterfaceName)
		if err != nil {
			return fmt.Errorf("interface name is invalid: %v", err)
		}
	}
	if node.TunQueues != nil && (*node.TunQueues < 1 || *node.TunQueues > 32) {
		return fmt.Errorf("tun_queues must be between 1 and 32, got %d", *node.TunQueues)
	}
	if node.TunTxQueueLen != nil && (*node.TunTxQueueLen < 0 || *node.TunTxQueueLen > 1<<20) {
		return fmt.Errorf("tun_txqueuelen must be between 0 and %d, got %d", 1<<20, *node.TunTxQueueLen)
	}
	if err := validateTunablesOverrides(node.Tunables); err != nil {
		return err
	}
	if node.ObservabilityAddr != "" {
		if _, _, err := net.SplitHostPort(node.ObservabilityAddr); err != nil {
			return fmt.Errorf("observability address must be a valid host:port: %v", err)
		}
	}
	if node.Dist != nil {
		if node.Dist.Key == (NyPublicKey{}) {
			return fmt.Errorf("dist.key must not be empty")
		}
		_, err := url.Parse(node.Dist.Url)
		if err != nil {
			return err
		}
	}
	if len(node.DnsResolvers) != 0 {
		for _, resolver := range node.DnsResolvers {
			if _, err := netip.ParseAddrPort(resolver); err != nil {
				return fmt.Errorf("dns resolver %s is not a valid ip:port: %v", resolver, err)
			}
		}
	}
	// validate prefixes
	for _, p := range append(node.UnexcludeIPs, node.ExcludeIPs...) {
		if !p.IsValid() {
			return fmt.Errorf("invalid prefix %s", p)
		}
	}
	// check that node is in central config
	if central != nil {
		centralNode := central.TryGetNode(node.Id)
		if centralNode == nil {
			return fmt.Errorf("node %s is not in central config", node.Id)
		}
		if node.NoTun && (len(centralNode.Addresses) != 0 || len(centralNode.Prefixes) != 0) {
			return fmt.Errorf("no_tun node %s cannot advertise addresses or prefixes", node.Id)
		}
	}
	return nil
}

func validateTunablesOverrides(o *TunablesOverrides) error {
	if o == nil {
		return nil
	}
	positive := map[string]*time.Duration{
		"probe_delay":                   o.ProbeDelay,
		"probe_recovery_delay":          o.ProbeRecoveryDelay,
		"probe_discovery_delay":         o.ProbeDiscoveryDelay,
		"gc_delay":                      o.GcDelay,
		"link_dead_threshold":           o.LinkDeadThreshold,
		"route_expiry_time":             o.RouteExpiryTime,
		"no_endpoint_warn_interval":     o.NoEndpointWarnInterval,
		"handshake_stale_debounce":      o.HandshakeStaleDebounce,
		"handshake_stale_warn_interval": o.HandshakeStaleWarnInterval,
	}
	for name, d := range positive {
		if d != nil && *d <= 0 {
			return fmt.Errorf("%s must be greater than 0, got %s", name, *d)
		}
	}
	if o.ProbeTimeout != nil && (*o.ProbeTimeout < 0 || *o.ProbeTimeout > 10*time.Second) {
		return fmt.Errorf("probe_timeout must be between 0 and 10s, got %s", *o.ProbeTimeout)
	}
	if o.LossPenaltyGain != nil && (*o.LossPenaltyGain < 0 || *o.LossPenaltyGain > 10) {
		return fmt.Errorf("loss_penalty_gain must be between 0 and 10, got %g", *o.LossPenaltyGain)
	}
	if o.LossDemotionThreshold != nil && (*o.LossDemotionThreshold < 0 || *o.LossDemotionThreshold >= 1) {
		return fmt.Errorf("loss_demotion_threshold must be in [0, 1), got %g", *o.LossDemotionThreshold)
	}
	if o.LinkSwitchDeadband != nil && *o.LinkSwitchDeadband < 1 {
		return fmt.Errorf("link_switch_deadband must be at least 1, got %g", *o.LinkSwitchDeadband)
	}
	if o.RouteSwitchHold != nil && *o.RouteSwitchHold < 0 {
		return fmt.Errorf("route_switch_hold must be at least 0, got %s", *o.RouteSwitchHold)
	}
	if o.HandshakeStaleWarn != nil && o.HandshakeStaleDemote != nil && *o.HandshakeStaleWarn > *o.HandshakeStaleDemote {
		return fmt.Errorf("handshake_stale_warn (%s) must not exceed handshake_stale_demote (%s)", *o.HandshakeStaleWarn, *o.HandshakeStaleDemote)
	}
	def := DefaultRouterTunables()
	ApplyTunablesOverrides(&def, o)
	if def.LinkDeadThreshold <= def.ProbeDelay {
		return fmt.Errorf("effective link_dead_threshold (%s) must be greater than probe_delay (%s)", def.LinkDeadThreshold, def.ProbeDelay)
	}
	return nil
}

func AddrToPrefix(addr netip.Addr) netip.Prefix {
	res, err := addr.Prefix(addr.BitLen())
	if err != nil {
		panic(err)
	}
	if !res.IsValid() {
		panic("invalid prefix")
	}
	return res
}

func CentralConfigValidator(cfg *CentralCfg) error {
	if cfg.Obf != nil {
		if err := cfg.Obf.Validate(); err != nil {
			return fmt.Errorf("invalid obf profile: %w", err)
		}
	}
	nodes := make([]string, 0)
	for _, node := range cfg.Routers {
		err := NameValidator(string(node.Id))
		if err != nil {
			return err
		}
		if slices.Contains(nodes, string(node.Id)) {
			return fmt.Errorf("duplicate router id %s", node.Id)
		}
		for _, endpoint := range node.Endpoints {
			if _, _, err := parseEndpoint(endpoint); err != nil {
				return fmt.Errorf("router %s has invalid endpoint: %w", node.Id, err)
			}
		}
		nodes = append(nodes, string(node.Id))
	}
	for _, node := range cfg.Clients {
		err := NameValidator(string(node.Id))
		if err != nil {
			return err
		}
		if slices.Contains(nodes, string(node.Id)) {
			return fmt.Errorf("duplicate client id %s", node.Id)
		}
		nodes = append(nodes, string(node.Id))
	}
	_, err := ParseGraph(cfg.Graph, nodes)
	if err != nil {
		return err
	}

	// ensure each node contains unique prefixes (anycast routing allows duplicate prefixes across nodes)
	for _, router := range cfg.Routers {
		routerPrefixes := make(map[netip.Prefix]struct{})
		for _, p := range router.Prefixes {
			if _, ok := routerPrefixes[p.GetPrefix()]; ok {
				return fmt.Errorf("router %s has duplicate prefix %s", router.Id, p)
			}
			routerPrefixes[p.GetPrefix()] = struct{}{}
		}
		for _, peer := range cfg.GetPeers(router.Id) {
			if cfg.IsClient(peer) {
				client := cfg.GetClient(peer)
				for _, cp := range client.Prefixes {
					if _, ok := routerPrefixes[cp.GetPrefix()]; ok {
						return fmt.Errorf("router %s has duplicate prefix %s (provided by client %s)", router.Id, cp, client.Id)
					}
					routerPrefixes[cp.GetPrefix()] = struct{}{}
				}
			}
		}
	}

	if cfg.Dist != nil {
		if cfg.Dist.Key == (NyPublicKey{}) {
			return fmt.Errorf("dist.key must not be empty")
		}
		// validate repos
		for _, repo := range cfg.Dist.Repos {
			_, err := url.Parse(repo)
			if err != nil {
				return err
			}
		}
	}
	// validate excludes
	for _, p := range cfg.ExcludeIPs {
		if !p.IsValid() {
			return fmt.Errorf("invalid prefix %s", p)
		}
	}
	// validate dynamic prefix ranges (design §4.6)
	for _, r := range cfg.DynamicPrefixRanges {
		if !r.IsValid() || r != r.Masked() {
			return fmt.Errorf("invalid dynamic prefix range %s", r)
		}
	}
	// validate prefixes
	phs := make([]PrefixHealthWrapper, 0)
	for _, c := range cfg.Clients {
		phs = append(phs, c.Prefixes...)
	}
	for _, c := range cfg.Routers {
		phs = append(phs, c.Prefixes...)
	}
	for _, p := range phs {
		if !p.GetPrefix().IsValid() {
			return fmt.Errorf("invalid prefix %s", p.GetPrefix())
		}
		switch v := p.PrefixHealth.(type) {
		case *StaticPrefixHealth:
			// ok
		case *PingPrefixHealth:
			if !v.Addr.IsValid() {
				return fmt.Errorf("invalid ping address %s for prefix %s", v.Addr, p.GetPrefix())
			}
			if v.Delay != nil && *v.Delay <= 0 {
				return fmt.Errorf("ping delay must be greater than 0 for prefix %s", p.GetPrefix())
			}
			if v.MaxFailures != nil && *v.MaxFailures <= 0 {
				return fmt.Errorf("ping max_failures must be greater than 0 for prefix %s", p.GetPrefix())
			}
		case *HTTPPrefixHealth:
			_, err := url.Parse(v.URL)
			if err != nil {
				return fmt.Errorf("invalid HTTP URL %s for prefix %s: %v", v.URL, p.GetPrefix(), err)
			}
			if v.Delay != nil && *v.Delay <= 0 {
				return fmt.Errorf("HTTP delay must be greater than 0 for prefix %s", p.GetPrefix())
			}
		default:
			return fmt.Errorf("unknown prefix health type for prefix %s", p.GetPrefix())
		}
	}

	// ensure that passive nodes only advertise static prefixes
	for _, client := range cfg.Clients {
		for _, prefix := range client.Prefixes {
			switch prefix.PrefixHealth.(type) {
			case *StaticPrefixHealth:
				// ok
			default:
				return fmt.Errorf("passive clients may not advertise non-static prefixes")
			}
		}
	}
	return nil
}
