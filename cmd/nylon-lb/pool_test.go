package main

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustAddr parses s as an IP address, failing the test on error.
func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	require.NoError(t, err)
	return addr
}

// takenOf builds an Allocate taken-set from string addresses.
func takenOf(addrs ...string) map[netip.Addr]struct{} {
	taken := make(map[netip.Addr]struct{}, len(addrs))
	for _, a := range addrs {
		taken[netip.MustParseAddr(a)] = struct{}{}
	}
	return taken
}

func TestPoolAllocateLowestFree(t *testing.T) {
	pool, err := ParsePool("10.0.0.0/29", nil)
	require.NoError(t, err)

	tests := []struct {
		name     string
		taken    map[netip.Addr]struct{}
		wantIP   netip.Addr
		wantOK   bool
		wantUsed int // allocations before the case when draining sequentially
	}{
		{
			name:   "empty pool hands out first usable address, not network",
			taken:  nil,
			wantIP: mustAddr(t, "10.0.0.1"),
			wantOK: true,
		},
		{
			name:   "skips taken addresses in order",
			taken:  takenOf("10.0.0.1", "10.0.0.2"),
			wantIP: mustAddr(t, "10.0.0.3"),
			wantOK: true,
		},
		{
			name:   "never hands out the broadcast address",
			taken:  takenOf("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"),
			wantIP: mustAddr(t, "10.0.0.6"),
			wantOK: true,
		},
		{
			name:   "all six usable addresses taken exhausts the pool",
			taken:  takenOf("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"),
			wantIP: netip.Addr{},
			wantOK: false,
		},
		{
			name:   "network and broadcast in taken still exhausts the pool",
			taken:  takenOf("10.0.0.0", "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6", "10.0.0.7"),
			wantIP: netip.Addr{},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, ok := pool.Allocate(tt.taken)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantIP, ip)
		})
	}

	// Draining sequentially yields each usable address exactly once, in
	// increasing order, then fails.
	taken := map[netip.Addr]struct{}{}
	for i := 1; i <= 6; i++ {
		ip, ok := pool.Allocate(taken)
		require.True(t, ok, "allocation %d of 6 usable addresses failed", i)
		assert.Equal(t, netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}), ip, "allocation %d", i)
		taken[ip] = struct{}{}
	}
	_, ok := pool.Allocate(taken)
	assert.False(t, ok, "seventh allocation must fail after all 6 usable addresses")
}

func TestPoolAllocateSkipsExcludes(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/29", []string{"192.0.2.1"})
	require.NoError(t, err)

	ip, ok := pool.Allocate(nil)
	assert.True(t, ok)
	assert.Equal(t, mustAddr(t, "192.0.2.2"), ip, "excluded .1 must be skipped")

	// With .2 and .3 taken, allocation lands on .4, still never on .1.
	ip, ok = pool.Allocate(takenOf("192.0.2.2", "192.0.2.3"))
	assert.True(t, ok)
	assert.Equal(t, mustAddr(t, "192.0.2.4"), ip)
}

func TestParsePoolRejects(t *testing.T) {
	tests := []struct {
		name    string
		cidr    string
		exclude []string
		wantErr string
	}{
		{
			name:    "IPv6 CIDR",
			cidr:    "2001:db8::/64",
			wantErr: "only IPv4 pools are supported",
		},
		{
			name:    "/31 has no usable host",
			cidr:    "10.0.0.0/31",
			wantErr: "leaves no usable host",
		},
		{
			name:    "/32 has no usable host",
			cidr:    "10.0.0.0/32",
			wantErr: "leaves no usable host",
		},
		{
			name:    "exclude outside the pool",
			cidr:    "10.0.0.0/29",
			exclude: []string{"10.0.1.1"},
			wantErr: `pool exclusion "10.0.1.1" is outside pool CIDR 10.0.0.0/29`,
		},
		{
			name:    "malformed CIDR",
			cidr:    "10.0.0.0/33",
			wantErr: "parsing pool CIDR",
		},
		{
			name:    "garbage CIDR",
			cidr:    "not-a-cidr",
			wantErr: "parsing pool CIDR",
		},
		{
			name:    "malformed exclude IP",
			cidr:    "10.0.0.0/29",
			exclude: []string{"10.0.0.256"},
			wantErr: `parsing pool exclusion "10.0.0.256"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, err := ParsePool(tt.cidr, tt.exclude)
			require.Error(t, err)
			assert.Nil(t, pool)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPoolContains(t *testing.T) {
	pool, err := ParsePool("10.110.0.0/24", nil)
	require.NoError(t, err)

	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{name: "first usable address", ip: "10.110.0.1", want: true},
		{name: "middle of range", ip: "10.110.0.128", want: true},
		{name: "last address (broadcast)", ip: "10.110.0.255", want: true},
		{name: "network address", ip: "10.110.0.0", want: true},
		{name: "one past broadcast", ip: "10.110.1.0", want: false},
		{name: "one before network", ip: "10.109.255.255", want: false},
		{name: "different family", ip: "2001:db8::1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pool.Contains(mustAddr(t, tt.ip)))
		})
	}
}

func TestParsePoolMasksPrefix(t *testing.T) {
	// A host bit set in the input is masked away: allocation operates on the
	// masked network.
	pool, err := ParsePool("10.0.0.77/29", nil)
	require.NoError(t, err)
	ip, ok := pool.Allocate(nil)
	assert.True(t, ok)
	assert.Equal(t, mustAddr(t, "10.0.0.73"), ip)
}

func TestParsePoolSetValid(t *testing.T) {
	pools, err := ParsePoolSet(
		[]string{"shared=192.0.2.0/29", "private=198.51.100.0/29"},
		[]string{"192.0.2.1", "198.51.100.2"},
	)
	require.NoError(t, err)

	assert.Equal(t, []string{"shared", "private"}, pools.Names(),
		"Names must preserve declaration order")

	shared, ok := pools.Get("shared")
	require.True(t, ok, "shared pool must resolve")
	ip, ok := shared.Allocate(takenOf("192.0.2.1", "192.0.2.2"))
	require.True(t, ok)
	assert.Equal(t, mustAddr(t, "192.0.2.3"), ip,
		"the containing pool's exclude (.1) must be skipped")

	_, ok = pools.Get("other")
	assert.False(t, ok, "unknown pool name must not resolve")

	// Any-pool membership spans every member.
	assert.True(t, pools.Contains(mustAddr(t, "192.0.2.4")))
	assert.True(t, pools.Contains(mustAddr(t, "198.51.100.7")))
	assert.False(t, pools.Contains(mustAddr(t, "203.0.113.1")))
}

func TestParsePoolSetRejects(t *testing.T) {
	tests := []struct {
		name    string
		specs   []string
		exclude []string
		wantErr string
	}{
		{
			name:    "no specs at all",
			specs:   nil,
			wantErr: "no pools configured: pass --pool name=cidr",
		},
		{
			name:    "unnamed spec",
			specs:   []string{"192.0.2.0/24"},
			wantErr: `must be name=cidr`,
		},
		{
			name:    "uppercase name",
			specs:   []string{"Shared=192.0.2.0/29"},
			wantErr: "pool name",
		},
		{
			name:    "name with a space",
			specs:   []string{"a b=192.0.2.0/29"},
			wantErr: "pool name",
		},
		{
			name:    "name starting with a dash",
			specs:   []string{"-shared=192.0.2.0/29"},
			wantErr: "pool name",
		},
		{
			name:    "duplicate name",
			specs:   []string{"shared=192.0.2.0/29", "shared=198.51.100.0/29"},
			wantErr: `duplicate pool name "shared"`,
		},
		{
			name:    "per-pool validation still fires on /31",
			specs:   []string{"shared=10.0.0.0/31"},
			wantErr: "leaves no usable host",
		},
		{
			name:    "per-pool validation still fires on IPv6",
			specs:   []string{"shared=2001:db8::/64"},
			wantErr: "only IPv4 pools are supported",
		},
		{
			name:    "exclude outside every pool",
			specs:   []string{"shared=192.0.2.0/29"},
			exclude: []string{"198.51.100.1"},
			wantErr: `exclude "198.51.100.1" is outside every pool`,
		},
		{
			name:    "malformed exclude IP",
			specs:   []string{"shared=192.0.2.0/29"},
			exclude: []string{"not-an-ip"},
			wantErr: `parsing pool exclusion "not-an-ip"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pools, err := ParsePoolSet(tt.specs, tt.exclude)
			require.Error(t, err)
			assert.Nil(t, pools)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPoolSetExcludeScopedToContainingPool(t *testing.T) {
	pools, err := ParsePoolSet(
		[]string{"shared=192.0.2.0/29", "private=198.51.100.0/29"},
		[]string{"192.0.2.1"},
	)
	require.NoError(t, err)

	shared, ok := pools.Get("shared")
	require.True(t, ok)
	private, ok := pools.Get("private")
	require.True(t, ok)

	ip, ok := shared.Allocate(nil)
	require.True(t, ok)
	assert.Equal(t, mustAddr(t, "192.0.2.2"), ip,
		"the excluded .1 must be skipped inside the containing pool")

	ip, ok = private.Allocate(nil)
	require.True(t, ok)
	assert.Equal(t, mustAddr(t, "198.51.100.1"), ip,
		"the exclude must not leak into the sibling pool")
}
