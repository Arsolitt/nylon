package core

import (
	"time"

	"github.com/encodeous/nylon/log"
	"github.com/encodeous/nylon/polyamide/device"
	"github.com/encodeous/nylon/state"
)

func nylonGc(n *Nylon) error {
	activeAddresses := make(map[string]struct{})
	for _, neigh := range n.RouterState.Neighbours {
		for _, endpoint := range neigh.Eps {
			link := endpoint.AsNylonEndpoint()
			if link.IsActive() {
				activeAddresses[link.Address] = struct{}{}
			}
		}
	}

	// scan for dead links
	for _, neigh := range n.RouterState.Neighbours {
		// filter dplinks
		count := 0
		for _, x := range neigh.Eps {
			x := x.AsNylonEndpoint()
			if !x.IsActive() {
				if _, activeElsewhere := activeAddresses[x.Address]; !activeElsewhere {
					n.EndpointResolver.Expire(x.Address)
				}
			}
			if x.IsAlive() {
				neigh.Eps[count] = x
				count++
			} else {
				n.Log.Debug("removed dead endpoint", "ep", x.Address, "to", neigh.Id)
			}
		}
		neigh.Eps = neigh.Eps[:count]
	}

	n.checkHandshakeStaleness(time.Now())

	err := n.GcRouter()
	if err != nil {
		return err
	}

	return nil
}

// checkHandshakeStaleness demotes neighbours whose WireGuard handshake is
// older than HandshakeStaleDemote (sustained for HandshakeStaleDebounce),
// warns past HandshakeStaleWarn, and clears the state once a fresh handshake
// lands. A zero last-handshake or one in the future (clock step) is skipped.
func (n *Nylon) checkHandshakeStaleness(now time.Time) {
	if n.Device == nil {
		return
	}
	for _, neigh := range n.RouterState.Neighbours {
		ncfg := n.TryGetNode(neigh.Id)
		if ncfg == nil {
			continue
		}
		peer := n.Device.LookupPeer(device.NoisePublicKey(ncfg.PubKey))
		if peer == nil {
			continue
		}
		lastHS := peer.Status().LatestHandshakeTime()
		if lastHS.IsZero() || lastHS.After(now) {
			continue
		}
		age := now.Sub(lastHS)
		if n.HandshakeStaleWarn > 0 && age > n.HandshakeStaleWarn && n.RouterState.WarnHandshakeStale(neigh.Id) {
			n.RouterEvent(log.EventHandshakeStale, "wireguard handshake stale", "neigh", neigh.Id, "age", age.Round(time.Millisecond))
		}
		demoted, recovered := advanceHsStale(neigh, now, age, n.HandshakeStaleDemote, n.HandshakeStaleDebounce)
		if demoted {
			n.RouterEvent(log.EventHandshakeStaleDemoted, "neighbour demoted for stale wireguard handshake", "neigh", neigh.Id, "age", age.Round(time.Millisecond))
		}
		if recovered {
			n.RouterEvent(log.EventHandshakeStaleRecovered, "stale wireguard handshake recovered", "neigh", neigh.Id, "age", age.Round(time.Millisecond))
		}
	}
}

// advanceHsStale updates the neighbour's handshake-staleness demotion state
// for the current handshake age and reports the transitions. demote > 0 &&
// age > demote arms HsStaleSince; the demotion lands once it has been
// sustained for debounce. Anything else (fresh handshake or stage disabled)
// clears the state; recovery is immediate, only the demotion debounces.
func advanceHsStale(neigh *state.Neighbour, now time.Time, age time.Duration, demote, debounce time.Duration) (demoted, recovered bool) {
	if demote > 0 && age > demote {
		if neigh.HsStaleSince.IsZero() {
			neigh.HsStaleSince = now
		}
		if now.Sub(neigh.HsStaleSince) >= debounce && !neigh.HsStale {
			neigh.HsStale = true
			demoted = true
		}
		return demoted, false
	}
	if neigh.HsStale {
		neigh.HsStale = false
		recovered = true
	}
	neigh.HsStaleSince = time.Time{}
	return false, recovered
}
