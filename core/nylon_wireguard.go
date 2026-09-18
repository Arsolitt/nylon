package core

import (
	"bufio"
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"slices"
	"strings"

	"github.com/encodeous/nylon/polyamide/conn"
	"github.com/encodeous/nylon/polyamide/device"
	"github.com/encodeous/nylon/polyamide/tun"
	"github.com/encodeous/nylon/state"
)

// resolveMTU returns the configured TUN MTU or the device default.
func resolveMTU(cfg *state.LocalCfg) int {
	if cfg.MTU != nil {
		return int(*cfg.MTU)
	}
	return device.DefaultMTU
}

// resolveTunOptions returns the configured TUN link options or the defaults.
func resolveTunOptions(cfg *state.LocalCfg) tun.CreateOptions {
	opts := tun.DefaultCreateOptions()
	if cfg.TunQueues != nil {
		opts.Queues = *cfg.TunQueues
	}
	if cfg.TunTxQueueLen != nil {
		opts.TxQueueLen = *cfg.TunTxQueueLen
	}
	if cfg.TunBackpressure != nil {
		opts.Backpressure = *cfg.TunBackpressure
	}
	return opts
}

// obfDeviceIPC renders the device-level AWG 2.0 UAPI knobs for the active
// central config. A nil profile yields the vanilla-compat default: AWG with
// every parameter unset picks message type 0 (PickOne over a zero range),
// which is NOT vanilla WireGuard — pinning H1..H4 to the WG message types
// preserves byte-level vanilla behavior for existing meshes and the
// version-skew window (design §2.4). I-packets are sender-local: only this
// node's own ObfPeerParams apply.
func obfDeviceIPC(obf *state.ObfProfile, local *state.ObfPeerParams) string {
	var b strings.Builder
	if obf == nil {
		b.WriteString("h1=1-1\nh2=2-2\nh3=3-3\nh4=4-4\n")
	} else {
		fmt.Fprintf(&b, "jc=%d\njmin=%d\njmax=%d\n", obf.Jc, obf.Jmin, obf.Jmax)
		fmt.Fprintf(&b, "s1=%d\ns2=%d\ns3=%d\ns4=%d\n", obf.S1, obf.S2, obf.S3, obf.S4)
		fmt.Fprintf(&b, "h1=%d-%d\n", obf.H1.Min, obf.H1.Max)
		fmt.Fprintf(&b, "h2=%d-%d\n", obf.H2.Min, obf.H2.Max)
		fmt.Fprintf(&b, "h3=%d-%d\n", obf.H3.Min, obf.H3.Max)
		fmt.Fprintf(&b, "h4=%d-%d\n", obf.H4.Min, obf.H4.Max)
	}
	if local != nil {
		for i, spec := range [5]string{local.I1, local.I2, local.I3, local.I4, local.I5} {
			if spec != "" {
				fmt.Fprintf(&b, "i%d=%s\n", i+1, spec)
			}
		}
	}
	return b.String()
}

func (n *Nylon) obfDeviceIPC() string {
	return obfDeviceIPC(n.Obf, n.GetNode(n.LocalCfg.Id).Obf)
}

func (n *Nylon) initWireGuard() error {
	dev, tdev, itfName, err := NewWireGuardDevice(n)
	if err != nil {
		return err
	}

	err = dev.Up()
	if err != nil {
		return err
	}

	n.Device = dev
	n.Tun = tdev
	n.Interface = itfName

	n.InstallTC()
	n.Log.Info("installed nylon traffic control filter for polysock")

	dev.IpcHandler["get=nylon\n"] = func(writer *bufio.ReadWriter) error {
		return HandleNylonIPC(n, writer)
	}

	// TODO: fully convert to code-based api
	err = dev.IpcSet(
		fmt.Sprintf(
			`private_key=%s
listen_port=%d
`,
			hex.EncodeToString(n.Key[:]),
			n.Port,
		) + n.obfDeviceIPC(),
	)
	if err != nil {
		return fmt.Errorf("failed to configure wg device: %v", err)
	}

	// add peers
	err = n.SyncWireGuard()
	if err != nil {
		return err
	}

	// configure system networking

	// run pre-up commands
	for _, cmd := range n.PreUp {
		err = ExecSplit(n.Log, cmd)
		if err != nil {
			n.Log.Error("failed to run pre-up command", "err", err)
		}
	}

	if !n.NoNetConfigure && !n.NoTun {
		for _, addr := range n.GetRouter(n.LocalCfg.Id).Addresses {
			err := ConfigureAlias(n.Log, itfName, addr)
			if err != nil {
				n.Log.Error("failed to configure alias", "err", err)
			} else if !slices.Contains(n.AppliedSystem.Aliases, addr) {
				n.AppliedSystem.Aliases = append(n.AppliedSystem.Aliases, addr)
			}
		}

		err = InitInterface(n.Log, itfName)
		if err != nil {
			return err
		}
	}

	// run post-up commands
	for _, cmd := range n.PostUp {
		err = ExecSplit(n.Log, cmd)
		if err != nil {
			n.Log.Error("failed to run post-up command", "err", err)
		}
	}

	// schedule application state reconciliation
	n.RepeatTask(func() error {
		if err := n.SyncApplicationState(); err != nil {
			n.Log.Warn("runtime reconciliation incomplete; will retry", "err", err)
		}
		return nil
	}, n.ProbeDelay)

	n.startTunStatsPolling()

	return nil
}

func (n *Nylon) cleanupWireGuard() error {
	// remove routes
	for _, route := range n.AppliedSystem.Routes {
		err := RemoveRoute(n.Log, n.Tun, n.Interface, route)
		if err != nil {
			n.Log.Error("failed to remove route", "err", err)
		}
	}
	for _, addr := range n.AppliedSystem.Aliases {
		err := RemoveAlias(n.Log, n.Interface, addr)
		if err != nil {
			n.Log.Error("failed to remove alias", "err", err)
		}
	}
	// run pre-down commands
	for _, cmd := range n.PreDown {
		err := ExecSplit(n.Log, cmd)
		if err != nil {
			n.Log.Error("failed to run pre-down command", "err", err)
		}
	}
	err := CleanupWireGuardDevice(n)
	if err != nil {
		return err
	}
	// run post-down commands
	for _, cmd := range n.PostDown {
		err = ExecSplit(n.Log, cmd)
		if err != nil {
			n.Log.Error("failed to run post-down command", "err", err)
		}
	}
	return nil
}

func (n *Nylon) SyncWireGuard() error {
	if n.Device == nil {
		return nil
	}
	if n.AppliedSystem.Peers == nil {
		n.AppliedSystem.Peers = make(map[state.NodeId]state.NyPublicKey)
	}

	// (re-)apply the AWG 2.0 obfuscation knobs; a config reload may switch the
	// S/H profile live. In-flight handshakes can fail for one rekey window —
	// maintenance-action semantics (design §2.4).
	if err := n.Device.IpcSet(n.obfDeviceIPC()); err != nil {
		return fmt.Errorf("failed to apply obf profile: %v", err)
	}
	desired := make(map[state.NodeId]state.NyPublicKey)
	for _, peer := range n.GetPeers(n.Id) {
		ncfg := n.GetNode(peer)
		desired[peer] = ncfg.PubKey
	}

	// Prepare every desired peer before removing any old peer. In particular,
	// public-key rotation must keep the old peer alive until the forwarding
	// table has been rebound to the replacement.
	for _, peer := range slices.Sorted(slices.Values(n.GetPeers(n.Id))) {
		ncfg := n.GetNode(peer)
		wgPeer := n.Device.LookupPeer(device.NoisePublicKey(ncfg.PubKey))
		if wgPeer == nil {
			n.Log.Debug("adding", "peer", peer)
			var err error
			wgPeer, err = n.Device.NewPeer(device.NoisePublicKey(ncfg.PubKey))
			if err != nil {
				return err
			}
			wgPeer.Start()
		}
		if n.IsClient(peer) {
			wgPeer.SetPreferRoaming(true)
		}
	}

	if err := n.syncWireGuardEndpoints(); err != nil {
		return err
	}

	// The forwarding table caches concrete peers for the one-lookup hot path.
	// Rebind every entry before stopping peers from the previous generation.
	n.rebindForwardingPeers()

	desiredKeys := make(map[state.NyPublicKey]struct{}, len(desired))
	for _, key := range desired {
		desiredKeys[key] = struct{}{}
	}
	for _, wgPeer := range n.Device.GetPeers() {
		key := state.NyPublicKey(wgPeer.GetPublicKey())
		if _, ok := desiredKeys[key]; ok {
			continue
		}
		n.Log.Debug("removing obsolete WireGuard peer", "key", key)
		n.Device.RemovePeer(wgPeer.GetPublicKey())
	}

	n.AppliedSystem.Peers = desired
	return nil
}

func (n *Nylon) syncWireGuardEndpoints() error {
	if n.Device == nil {
		return nil
	}
	dev := n.Device

	// configure endpoints
	for _, peer := range slices.Sorted(slices.Values(n.GetPeers(n.Id))) {
		if n.IsClient(peer) {
			continue
		}
		pcfg := n.GetRouter(peer)
		nhNeigh := n.RouterState.GetNeighbour(peer)
		eps := make([]conn.Endpoint, 0)

		if nhNeigh != nil {
			links := slices.Clone(nhNeigh.Eps)
			slices.SortStableFunc(links, func(a, b state.Endpoint) int {
				return cmp.Compare(a.Metric(), b.Metric())
			})
			for _, ep := range links {
				nep, err := ep.AsNylonEndpoint().GetWgEndpoint(n.Device, n.EndpointResolver)
				if err != nil {
					continue
				}
				eps = append(eps, nep)
			}
		}

		wgPeer := dev.LookupPeer(device.NoisePublicKey(pcfg.PubKey))
		if wgPeer != nil {
			wgPeer.SetEndpoints(eps)
		}
	}

	return nil
}

func (n *Nylon) SyncSystemState() error {
	if n.NoNetConfigure || n.NoTun {
		return nil
	}
	return errors.Join(n.syncAliases(), n.syncSystemRoutes())
}

func (n *Nylon) syncAliases() error {
	desired := n.GetRouter(n.LocalCfg.Id).Addresses
	applied := slices.Clone(n.AppliedSystem.Aliases)
	var syncErr error
	// we must first add the new alias before removing the old ones, else the system might flush our routes
	for _, newEntry := range desired {
		if !slices.Contains(applied, newEntry) {
			n.Log.Debug("installing alias", "addr", newEntry.String())
			err := ConfigureAlias(n.Log, n.Interface, newEntry)
			if err != nil {
				n.Log.Error("failed to configure alias", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("install alias %s: %w", newEntry, err))
				continue
			}
			applied = append(applied, newEntry)
		}
	}
	hadAliases := len(applied) != 0
	for _, oldEntry := range slices.Clone(applied) {
		if !slices.Contains(desired, oldEntry) {
			n.Log.Debug("removing old alias", "addr", oldEntry.String())
			err := RemoveAlias(n.Log, n.Interface, oldEntry)
			if err != nil {
				n.Log.Error("failed to remove alias", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("remove alias %s: %w", oldEntry, err))
				continue
			}
			applied = slices.DeleteFunc(applied, func(addr netip.Addr) bool {
				return addr == oldEntry
			})
		}
	}
	// special case for linux: if all aliases are removed, the kernel will also flush the routes
	if hadAliases && len(applied) == 0 && runtime.GOOS == "linux" {
		n.AppliedSystem.Routes = nil
	}
	n.AppliedSystem.Aliases = applied
	return syncErr
}

func (n *Nylon) syncSystemRoutes() error {
	newEntries := n.ComputeSysRouteTable()
	applied := slices.Clone(n.AppliedSystem.Routes)
	var syncErr error
	// Install new routes before removing old ones so a partial reconciliation
	// preserves as much connectivity as possible.
	for _, newEntry := range newEntries {
		if !slices.Contains(applied, newEntry) {
			// install route
			n.Log.Debug("installing new route", "prefix", newEntry.String())
			err := ConfigureRoute(n.Log, n.Tun, n.Interface, newEntry)
			if err != nil {
				n.Log.Error("failed to configure route", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("install route %s: %w", newEntry, err))
				continue
			}
			applied = append(applied, newEntry)
		}
	}
	for _, oldEntry := range slices.Clone(applied) {
		if !slices.Contains(newEntries, oldEntry) {
			// uninstall route
			n.Log.Debug("removing old route", "prefix", oldEntry.String())
			err := RemoveRoute(n.Log, n.Tun, n.Interface, oldEntry)
			if err != nil {
				n.Log.Error("failed to remove route", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("remove route %s: %w", oldEntry, err))
				continue
			}
			applied = slices.DeleteFunc(applied, func(prefix netip.Prefix) bool {
				return prefix == oldEntry
			})
		}
	}
	n.AppliedSystem.Routes = applied
	return syncErr
}
