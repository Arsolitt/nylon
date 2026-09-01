package main

import (
	"fmt"
	"net/netip"
)

// Pool is a pure IPv4 address allocator for LoadBalancer Service ingress IPs.
// It holds no allocation state of its own: the caller derives the taken set
// from the live Services on every reconcile and passes it to Allocate.
type Pool struct {
	cidr     netip.Prefix
	excluded map[netip.Addr]struct{}
}

// ParsePool parses an IPv4 CIDR into a Pool. The prefix is masked, the network
// and broadcast addresses are implicitly unallocatable, and excludes are
// addresses that must never be handed out (e.g. a stand's probe IP).
func ParsePool(cidr string, excludes []string) (*Pool, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("parsing pool CIDR %q: %w", cidr, err)
	}
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("pool CIDR %q: only IPv4 pools are supported", cidr)
	}
	prefix = prefix.Masked()
	if prefix.Bits() > 30 {
		return nil, fmt.Errorf("pool CIDR %q: prefix length %d leaves no usable host once the network and broadcast addresses are excluded", cidr, prefix.Bits())
	}
	excluded := make(map[netip.Addr]struct{}, len(excludes))
	for _, ex := range excludes {
		addr, err := netip.ParseAddr(ex)
		if err != nil {
			return nil, fmt.Errorf("parsing pool exclusion %q: %w", ex, err)
		}
		if !prefix.Contains(addr) {
			return nil, fmt.Errorf("pool exclusion %q is outside pool CIDR %s", ex, prefix)
		}
		excluded[addr] = struct{}{}
	}
	return &Pool{cidr: prefix, excluded: excluded}, nil
}

// Contains reports whether ip falls inside the pool's CIDR range. This is a
// plain range check: the network and broadcast addresses count as inside;
// Allocate is what keeps them from ever being handed out.
func (p *Pool) Contains(ip netip.Addr) bool {
	return p.cidr.Contains(ip)
}

// Allocate returns the lowest address in the pool that is neither implicitly
// reserved (network + broadcast), explicitly excluded, nor already taken.
// Allocation state is DERIVED from live Services by the caller and never
// stored, so Allocate is a pure function of taken.
func (p *Pool) Allocate(taken map[netip.Addr]struct{}) (netip.Addr, bool) {
	network := p.cidr.Addr().As4()
	hostBits := uint(32 - p.cidr.Bits())
	broadcast := networkUint(network) | uint32(uint64(1)<<hostBits-1)
	for u := networkUint(network) + 1; u < broadcast; u++ {
		addr := netip.AddrFrom4([4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)})
		if _, bad := p.excluded[addr]; bad {
			continue
		}
		if _, bad := taken[addr]; bad {
			continue
		}
		return addr, true
	}
	return netip.Addr{}, false
}

// networkUint reinterprets a 4-byte IPv4 address as a big-endian uint32.
func networkUint(b [4]byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
