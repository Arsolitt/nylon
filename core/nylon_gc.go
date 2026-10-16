package core

import (
	"time"

	"github.com/encodeous/nylon/log"
	"github.com/encodeous/nylon/polyamide/device"
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
		if n.HandshakeStaleDemote > 0 && age > n.HandshakeStaleDemote {
			if neigh.HsStaleSince.IsZero() {
				neigh.HsStaleSince = now
			}
			if now.Sub(neigh.HsStaleSince) >= n.HandshakeStaleDebounce {
				neigh.HsStale = true
			}
		} else {
			neigh.HsStale = false
			neigh.HsStaleSince = time.Time{}
		}
	}
}
