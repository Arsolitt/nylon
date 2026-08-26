package core

import (
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/encodeous/nylon/state"
	"github.com/stretchr/testify/assert"
)

func staticDynPrefix(prefix netip.Prefix, metric uint32) state.PrefixHealthWrapper {
	return state.PrefixHealthWrapper{PrefixHealth: &state.StaticPrefixHealth{
		Prefix: prefix,
		Metric: metric,
	}}
}

func TestParseDynamicPrefixFileValidEntries(t *testing.T) {
	data := `{
		"version": 1,
		"prefixes": [
			{"type": "static", "prefix": "10.42.1.0/24", "metric": 5},
			{"type": "static", "prefix": "10.42.2.0/24"},
			{"type": "ping", "prefix": "10.1.0.0/24", "addr": "10.1.0.8", "delay": "10s", "max_failures": 3, "bind_if": "eth0", "metric": 5},
			{"type": "http", "prefix": "10.2.0.0/24", "url": "http://example.com/healthz", "delay": "1m30s", "metric": 5}
		]
	}`

	entries, errs := parseDynamicPrefixFile("valid.json", []byte(data))
	assert.Empty(t, errs)
	assert.Len(t, entries, 4)

	static, ok := entries[0].PrefixHealth.(*state.StaticPrefixHealth)
	assert.True(t, ok)
	assert.Equal(t, netip.MustParsePrefix("10.42.1.0/24"), static.Prefix)
	assert.Equal(t, uint32(5), static.Metric)

	staticDefault, ok := entries[1].PrefixHealth.(*state.StaticPrefixHealth)
	assert.True(t, ok)
	assert.Equal(t, uint32(0), staticDefault.Metric)

	ping, ok := entries[2].PrefixHealth.(*state.PingPrefixHealth)
	assert.True(t, ok)
	assert.Equal(t, netip.MustParsePrefix("10.1.0.0/24"), ping.Prefix)
	assert.Equal(t, netip.MustParseAddr("10.1.0.8"), ping.Addr)
	requireDuration(t, ping.Delay, 10*time.Second)
	requireInt(t, ping.MaxFailures, 3)
	assert.Equal(t, "eth0", ping.BindIf)
	requireMetric(t, ping.Metric, 5)

	httpEntry, ok := entries[3].PrefixHealth.(*state.HTTPPrefixHealth)
	assert.True(t, ok)
	assert.Equal(t, netip.MustParsePrefix("10.2.0.0/24"), httpEntry.Prefix)
	assert.Equal(t, "http://example.com/healthz", httpEntry.URL)
	requireDuration(t, httpEntry.Delay, 90*time.Second)
	requireMetric(t, httpEntry.Metric, 5)
}

func TestParseDynamicPrefixFileSkipsWholeFile(t *testing.T) {
	for name, data := range map[string]string{
		"bad json":      `{`,
		"not an object": `[1, 2, 3]`,
		"version 2":     `{"version": 2, "prefixes": [{"type": "static", "prefix": "10.0.0.0/24"}]}`,
		"no version":    `{"prefixes": [{"type": "static", "prefix": "10.0.0.0/24"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			entries, errs := parseDynamicPrefixFile("bad.json", []byte(data))
			assert.Empty(t, entries)
			assert.Len(t, errs, 1)
			assert.Contains(t, errs[0].Error(), "skipping file")
		})
	}
}

func TestParseDynamicPrefixFileRejectsInvalidEntries(t *testing.T) {
	data := `{
		"version": 1,
		"prefixes": [
			{"type": "static", "prefix": "10.0.0.0/24"},
			{"type": "gre", "prefix": "10.0.1.0/24"},
			{"type": "static", "prefix": "10.0.0.1/24"},
			{"type": "static", "prefix": "not-a-prefix"},
			{"type": "ping", "prefix": "10.0.2.0/24"},
			{"type": "ping", "prefix": "10.0.3.0/24", "addr": "not-an-addr"},
			{"type": "ping", "prefix": "10.0.4.0/24", "addr": "10.0.4.8", "delay": "fast"},
			{"type": "http", "prefix": "10.0.5.0/24"},
			{"type": "http", "prefix": "10.0.6.0/24", "url": "http://example.com/", "delay": "nope"}
		]
	}`

	entries, errs := parseDynamicPrefixFile("mixed.json", []byte(data))
	assert.Len(t, entries, 1)
	assert.Len(t, errs, 8)
	assert.Equal(t, netip.MustParsePrefix("10.0.0.0/24"), entries[0].GetPrefix())
}

func TestScanDynamicPrefixDir(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("empty dir", func(t *testing.T) {
		assert.Empty(t, scanDynamicPrefixDir(log, t.TempDir()))
	})

	t.Run("missing dir", func(t *testing.T) {
		assert.Nil(t, scanDynamicPrefixDir(log, filepath.Join(t.TempDir(), "missing")))
	})

	t.Run("conflict, union and filtering", func(t *testing.T) {
		dir := t.TempDir()
		// non-json files must be ignored
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`{"version":1,"prefixes":[]}`), 0o644))
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "10-x.json.swp"), []byte("junk"), 0o644))
		// lexicographic conflict: a.json wins over b.json for 10.0.0.0/24
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"version":1,"prefixes":[{"type":"static","prefix":"10.0.0.0/24","metric":7}]}`), 0o644))
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "b.json"), []byte(`{"version":1,"prefixes":[{"type":"static","prefix":"10.0.0.0/24","metric":9},{"type":"static","prefix":"10.0.1.0/24","metric":3}]}`), 0o644))
		// structurally invalid files are skipped, others still apply
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "c.json"), []byte(`{`), 0o644))
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "d.json"), []byte(`{"version":1,"prefixes":[{"type":"static","prefix":"10.0.2.0/24"}]}`), 0o644))

		entries := scanDynamicPrefixDir(log, dir)
		assert.Len(t, entries, 3)

		metric, ok := entries[0].StaticMetric()
		assert.True(t, ok)
		assert.Equal(t, uint32(7), metric, "a.json must win the 10.0.0.0/24 conflict")
		assert.Equal(t, netip.MustParsePrefix("10.0.0.0/24"), entries[0].GetPrefix())
		assert.Equal(t, netip.MustParsePrefix("10.0.1.0/24"), entries[1].GetPrefix())
		assert.Equal(t, netip.MustParsePrefix("10.0.2.0/24"), entries[2].GetPrefix())
	})
}

func TestInjectDynamicPrefixes(t *testing.T) {
	t.Run("appends non-conflicting and rejects central conflicts", func(t *testing.T) {
		centralPrefix := netip.MustParsePrefix("10.0.0.0/24")
		dynamicPrefix := netip.MustParsePrefix("10.9.0.0/24")
		n := testNylonWithPrefixes(staticDynPrefix(centralPrefix, 42))
		n.dynamicPrefixes = []state.PrefixHealthWrapper{
			staticDynPrefix(centralPrefix, 7), // conflicts with central; central wins
			staticDynPrefix(dynamicPrefix, 5),
		}

		n.injectDynamicPrefixes(&n.CentralCfg)

		node := n.CentralCfg.GetNode(n.LocalCfg.Id)
		assert.Len(t, node.Prefixes, 2)
		metric, ok := node.Prefixes[0].StaticMetric()
		assert.True(t, ok)
		assert.Equal(t, uint32(42), metric, "central metric must be preserved on conflict")
		metric, ok = node.Prefixes[1].StaticMetric()
		assert.True(t, ok)
		assert.Equal(t, uint32(5), metric)
		assert.Equal(t, dynamicPrefix, node.Prefixes[1].GetPrefix())
	})

	t.Run("nil local node is a no-op", func(t *testing.T) {
		n := testNylonWithPrefixes()
		n.dynamicPrefixes = []state.PrefixHealthWrapper{staticDynPrefix(netip.MustParsePrefix("10.9.0.0/24"), 5)}
		empty := state.CentralCfg{}
		n.injectDynamicPrefixes(&empty) // must not panic
		assert.Empty(t, empty.Routers)
	})
}

func TestApplyCentralConfigDynamicPrefixRoundTrip(t *testing.T) {
	vip := netip.MustParsePrefix("10.99.0.1/32")
	n := testNylonWithPrefixes()
	n.LocalCfg.DynamicPrefixesDir = t.TempDir() // enables injection
	n.dynamicPrefixes = []state.PrefixHealthWrapper{staticDynPrefix(vip, 0)}

	result, err := n.ApplyCentralConfig(&n.CentralCfg)
	assert.NoError(t, err)
	assert.Equal(t, ApplyApplied, result)

	// the committed config carries the dynamic prefix and advertises it
	node := n.CentralCfg.GetNode(n.LocalCfg.Id)
	found := false
	for _, p := range node.Prefixes {
		if p.GetPrefix() == vip {
			found = true
		}
	}
	assert.True(t, found, "committed config must contain the dynamic prefix")
	_, advertised := n.RouterState.Advertised[vip]
	assert.True(t, advertised, "dynamic prefix must be advertised")

	// the pristine snapshot stays free of dynamic state
	assert.NotNil(t, n.centralCfgPristine)
	for _, p := range n.centralCfgPristine.GetNode(n.LocalCfg.Id).Prefixes {
		assert.NotEqual(t, vip, p.GetPrefix())
	}

	// steady state: re-applying the pristine config with unchanged files is a noop
	result, err = n.ApplyCentralConfig(n.centralCfgPristine)
	assert.NoError(t, err)
	assert.Equal(t, ApplyNoop, result)
}

func requireDuration(t *testing.T, got *time.Duration, want time.Duration) {
	t.Helper()
	if assert.NotNil(t, got) {
		assert.Equal(t, want, *got)
	}
}

func requireInt(t *testing.T, got *int, want int) {
	t.Helper()
	if assert.NotNil(t, got) {
		assert.Equal(t, want, *got)
	}
}

func requireMetric(t *testing.T, got *uint32, want uint32) {
	t.Helper()
	if assert.NotNil(t, got) {
		assert.Equal(t, want, *got)
	}
}

func TestCheckPrefixDynamicRanges(t *testing.T) {
	centralPrefix := netip.MustParsePrefix("10.0.0.0/24")
	n := testNylonWithPrefixes(staticDynPrefix(centralPrefix, 0))
	n.router.log = n.Log // checkPrefix warns through the router logger

	// exact central prefixes are always accepted
	assert.True(t, n.checkPrefix(centralPrefix))

	// without ranges, unknown prefixes are rejected (fail-closed default)
	assert.False(t, n.checkPrefix(netip.MustParsePrefix("10.99.0.1/32")))

	n.CentralCfg.DynamicPrefixRanges = []netip.Prefix{netip.MustParsePrefix("10.99.0.0/24")}

	// subnets of a declared range are accepted
	assert.True(t, n.checkPrefix(netip.MustParsePrefix("10.99.0.0/24")))
	assert.True(t, n.checkPrefix(netip.MustParsePrefix("10.99.0.1/32")))
	// supernets of a declared range are not
	assert.False(t, n.checkPrefix(netip.MustParsePrefix("10.99.0.0/16")))
	// prefixes outside the range are still rejected
	assert.False(t, n.checkPrefix(netip.MustParsePrefix("10.99.1.0/24")))
}
