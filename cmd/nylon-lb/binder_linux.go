//go:build linux

package main

import (
	"fmt"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"go4.org/netipx"
)

// netlinkBinder is the production AddrBinder: it keeps one /32 per announced
// ingress IP on a local interface (lo by default) via netlink, so the node
// answers traffic for the addresses it announces. It requires root
// (CAP_NET_ADMIN), matching the root DaemonSet/systemd deployment.
type netlinkBinder struct {
	linkName string
	pools    *PoolSet
}

// newNetlinkBinder binds announced /32s on linkName (netlink is Linux-only;
// other platforms get a refusing stub in binder_other.go).
func newNetlinkBinder(linkName string, pools *PoolSet) (AddrBinder, error) {
	return &netlinkBinder{linkName: linkName, pools: pools}, nil
}

// Ensure idempotently replaces the /32 for ip on the bound interface.
func (b *netlinkBinder) Ensure(ip netip.Addr) error {
	link, err := netlink.LinkByName(b.linkName)
	if err != nil {
		return fmt.Errorf("link %s: %w", b.linkName, err)
	}
	return netlink.AddrReplace(link, &netlink.Addr{
		IPNet: netipx.PrefixIPNet(netip.PrefixFrom(ip, 32).Masked()),
		Label: "",
	})
}

// Remove deletes the /32 for ip if it is present on the bound interface; an
// absent address is success, keeping reconcile idempotent.
func (b *netlinkBinder) Remove(ip netip.Addr) error {
	link, err := netlink.LinkByName(b.linkName)
	if err != nil {
		return fmt.Errorf("link %s: %w", b.linkName, err)
	}
	addrs, err := netlink.AddrList(link, nl.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("listing addresses on %s: %w", b.linkName, err)
	}
	for _, addr := range addrs {
		if binderAddrIs32(addr, ip) {
			return netlink.AddrDel(link, &addr)
		}
	}
	return nil
}

// ListInPool returns every IPv4 address on the bound interface that falls
// inside any configured pool — the set reconcile sweeps for drift.
func (b *netlinkBinder) ListInPool() ([]netip.Addr, error) {
	link, err := netlink.LinkByName(b.linkName)
	if err != nil {
		return nil, fmt.Errorf("link %s: %w", b.linkName, err)
	}
	addrs, err := netlink.AddrList(link, nl.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("listing addresses on %s: %w", b.linkName, err)
	}
	var ips []netip.Addr
	for _, addr := range addrs {
		if addr.IPNet == nil {
			continue
		}
		prefix, ok := netipx.FromStdIPNet(addr.IPNet)
		if !ok || !prefix.Addr().Is4() {
			continue
		}
		if b.pools.Contains(prefix.Addr()) {
			ips = append(ips, prefix.Addr())
		}
	}
	return ips, nil
}

// binderAddrIs32 reports whether a listed netlink address is exactly the /32
// of ip.
func binderAddrIs32(addr netlink.Addr, ip netip.Addr) bool {
	if addr.IPNet == nil {
		return false
	}
	prefix, ok := netipx.FromStdIPNet(addr.IPNet)
	return ok && prefix.Bits() == 32 && prefix.Addr() == ip
}
