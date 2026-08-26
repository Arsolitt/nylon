//go:build integration

package integration

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/encodeous/nylon/core"
	"github.com/encodeous/nylon/state"
	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
)

// TestDynamicPrefixAnnounceAndWithdraw exercises the prefixes.d contract end
// to end (design §4.5): writing a file to the watched directory must announce
// the prefix through the routing protocol, and deleting it must retract the
// announcement again.
func TestDynamicPrefixAnnounceAndWithdraw(t *testing.T) {
	defer goleak.VerifyNone(t)

	vh := &VirtualHarness{}
	a1 := "192.168.10.1:1234"
	vh.NewNode("a", "10.0.0.1/32")
	b1 := "192.168.10.2:1234"
	vh.NewNode("b", "10.0.0.2/32")
	c1 := "192.168.10.3:1234"
	vh.NewNode("c", "10.0.0.3/32")
	vh.Central.Graph = []string{
		"a, b, c",
	}
	vh.Endpoints = map[string]state.NodeId{
		a1: "a",
		b1: "b",
		c1: "c",
	}
	vh.AddLink(a1, b1)
	vh.AddLink(b1, a1)
	vh.AddLink(a1, c1)
	vh.AddLink(c1, a1)
	vh.AddLink(b1, c1)
	vh.AddLink(c1, b1)

	vh.Central.DynamicPrefixRanges = []netip.Prefix{netip.MustParsePrefix("10.99.0.0/24")}
	dir := t.TempDir()
	vh.Local[vh.IndexOf("a")].DynamicPrefixesDir = dir

	errs := vh.Start()
	defer vh.Stop()

	a := vh.Nylons[vh.IndexOf("a")].Load()
	b := vh.Nylons[vh.IndexOf("b")].Load()

	vip := netip.MustParsePrefix("10.99.0.1/32")

	// announce
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "10-vip.json"),
		[]byte(`{"version":1,"prefixes":[{"type":"static","prefix":"10.99.0.1/32","metric":0}]}`), 0o644))
	waitForDynamicPrefix(t, errs, a, b, vip, true)

	// withdraw
	assert.NoError(t, os.Remove(filepath.Join(dir, "10-vip.json")))
	waitForDynamicPrefix(t, errs, a, b, vip, false)
}

// waitForDynamicPrefix polls (via Dispatch, so reads race-free) until node a
// has/hasn't advertised the prefix and node b has/hasn't selected a route
// for it.
func waitForDynamicPrefix(t *testing.T, errs <-chan error, a, b *core.Nylon, prefix netip.Prefix, want bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errs:
			t.Fatal(err)
		default:
		}
		if dynamicPrefixState(a, b, prefix) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for dynamic prefix %s want=%v", prefix, want)
}

func dynamicPrefixState(a, b *core.Nylon, prefix netip.Prefix) bool {
	aState := make(chan bool, 1)
	bState := make(chan bool, 1)
	a.Dispatch(func() error {
		_, ok := a.RouterState.Advertised[prefix]
		aState <- ok
		return nil
	})
	b.Dispatch(func() error {
		_, ok := b.RouterState.Routes[prefix]
		bState <- ok
		return nil
	})
	select {
	case advertised := <-aState:
		if !advertised {
			return false
		}
	case <-time.After(2 * time.Second):
		return false
	}
	select {
	case routed := <-bState:
		return routed
	case <-time.After(2 * time.Second):
		return false
	}
}
