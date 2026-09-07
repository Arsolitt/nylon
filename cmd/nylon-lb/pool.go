package main

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// Pool is a pure IPv4 address allocator for LoadBalancer Service ingress IPs.
// It holds no allocation state of its own: the caller derives the taken set
// from the live Services on every reconcile and passes it to Allocate.
type Pool struct {
	name     string // pool name as configured; set by ParsePoolSet only
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

// poolNameRe is the shape of a legal --pool name: lowercase alphanumerics
// and inner dashes, 1-63 characters, mirroring a DNS label relaxed to allow
// leading digits.
var poolNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// PoolSet is the configured set of named pools. The controller selects one
// member pool per Service via the nylon.io/lb-pool annotation; membership
// checks (Contains) span every member, so release and drift logic treat all
// pools uniformly and the global taken set keeps overlapping pools safe.
type PoolSet struct {
	order  []string // declaration order, for deterministic logs
	byName map[string]*Pool
}

// ParsePoolSet parses repeatable --pool specs of the form name=cidr into a
// PoolSet. Each CIDR goes through the single-pool validation (IPv4, masked,
// /30 or shorter); overlapping pools are allowed. Every exclude must fall
// inside at least one pool and is registered in every pool containing it.
func ParsePoolSet(specs, excludes []string) (*PoolSet, error) {
	if len(specs) == 0 {
		return nil, errors.New("no pools configured: pass --pool name=cidr")
	}
	set := &PoolSet{byName: make(map[string]*Pool, len(specs))}
	for _, spec := range specs {
		name, cidr, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, fmt.Errorf("pool spec %q must be name=cidr (e.g. shared=10.110.0.0/24)", spec)
		}
		if !poolNameRe.MatchString(name) {
			return nil, fmt.Errorf("pool name %q in %q must match %s", name, spec, poolNameRe)
		}
		if _, dup := set.byName[name]; dup {
			return nil, fmt.Errorf("duplicate pool name %q", name)
		}
		pool, err := ParsePool(cidr, nil)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", name, err)
		}
		pool.name = name
		set.byName[name] = pool
		set.order = append(set.order, name)
	}
	for _, ex := range excludes {
		addr, err := netip.ParseAddr(ex)
		if err != nil {
			return nil, fmt.Errorf("parsing pool exclusion %q: %w", ex, err)
		}
		contained := false
		for _, name := range set.order {
			if pool := set.byName[name]; pool.Contains(addr) {
				pool.exclude(addr)
				contained = true
			}
		}
		if !contained {
			return nil, fmt.Errorf("exclude %q is outside every pool", ex)
		}
	}
	return set, nil
}

// Get returns the member pool named name and whether it exists.
func (p *PoolSet) Get(name string) (*Pool, bool) {
	pool, ok := p.byName[name]
	return pool, ok
}

// Contains reports whether ip falls inside ANY member pool.
func (p *PoolSet) Contains(ip netip.Addr) bool {
	for _, name := range p.order {
		if p.byName[name].Contains(ip) {
			return true
		}
	}
	return false
}

// Names returns the configured pool names in declaration order.
func (p *PoolSet) Names() []string {
	return slices.Clone(p.order)
}

// exclude registers addr as never allocatable in this pool; ParsePoolSet
// uses it to push each --exclude address into every pool containing it.
func (p *Pool) exclude(addr netip.Addr) {
	p.excluded[addr] = struct{}{}
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
