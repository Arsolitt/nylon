//go:build integration

package integration

import (
	"bytes"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/encodeous/nylon/core"
	"github.com/encodeous/nylon/perf"
	"github.com/encodeous/nylon/polyamide/tun/tuntest"
	"github.com/encodeous/nylon/state"
	"go.uber.org/goleak"
)

const (
	dupNodeAAddr    = "10.99.0.1"
	dupSettleWindow = 300 * time.Millisecond
)

// deliveredPacket is one frame the in-memory network handed to a node's TUN
// side. The hook sees the L3 payload after the IPv4 header, so the payload
// bytes are what duplication must preserve.
type deliveredPacket struct {
	src  netip.Addr
	dst  netip.Addr
	data []byte
}

type packetCapture struct {
	mu      sync.Mutex
	packets []deliveredPacket
	signal  chan struct{}
}

func newPacketCapture() *packetCapture {
	return &packetCapture{signal: make(chan struct{}, 1)}
}

func (c *packetCapture) onNode(want state.NodeId) PacketFilter {
	return func(node state.NodeId, src, dst netip.Addr, data []byte) bool {
		if node == want {
			c.mu.Lock()
			c.packets = append(c.packets, deliveredPacket{src: src, dst: dst, data: bytes.Clone(data)})
			c.mu.Unlock()
			select {
			case c.signal <- struct{}{}:
			default:
			}
		}
		return true
	}
}

func (c *packetCapture) snapshot() []deliveredPacket {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.packets)
}

// TestSinglePacketNotDuplicated injects one packet that must cross the mesh
// exactly once: one TUN read on the sender, one delivery on the receiver.
func TestSinglePacketNotDuplicated(t *testing.T) {
	defer goleak.VerifyNone(t)
	assertSingleDelivery(t, "10.99.0.2/32", "10.99.0.2")
}

// TestBounceNotDuplicated injects one packet into a prefix the receiving node
// owns, so the receiving side turns it back to its own TUN instead of
// forwarding it, and pins the same one-copy invariant on that path.
func TestBounceNotDuplicated(t *testing.T) {
	defer goleak.VerifyNone(t)
	assertSingleDelivery(t, "10.99.1.0/24", "10.99.1.2")
}

func assertSingleDelivery(t *testing.T, bPrefix, dst string) {
	t.Helper()

	tunables := state.DefaultRouterTunables()
	tunables.ProbeDelay /= 3
	tunables.RouteUpdateDelay /= 3

	vh := &VirtualHarness{Tunables: &tunables}
	a1 := "192.168.1.10:1234"
	vh.NewNode("a", dupNodeAAddr+"/32")
	b1 := "192.168.1.11:1234"
	vh.NewNode("b", bPrefix)
	vh.Central.Graph = []string{"a, b"}
	vh.Endpoints = map[string]state.NodeId{a1: "a", b1: "b"}
	vh.AddLink(a1, b1)
	vh.AddLink(b1, a1)

	errs := vh.Start()
	defer vh.Stop()

	a := vh.Nylons[vh.IndexOf("a")].Load()
	b := vh.Nylons[vh.IndexOf("b")].Load()
	awaitRoute(t, errs, a, netip.MustParsePrefix(bPrefix))
	awaitRoute(t, errs, b, netip.MustParsePrefix(dupNodeAAddr+"/32"))

	capture := newPacketCapture()
	vh.Net.SelfHandler = capture.onNode("b")

	ping := tuntest.Ping(netip.MustParseAddr(dst), netip.MustParseAddr(dupNodeAAddr))
	const ipv4HeaderLen = 20
	payload := ping[ipv4HeaderLen:]

	readsBefore := perf.TunReadFramesTotal.Load()
	vh.Net.Send("a", dupNodeAAddr, dst, payload, 64)

	select {
	case <-capture.signal:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the injected packet to be delivered")
	case err := <-errs:
		t.Fatalf("harness error: %v", err)
	}
	// No further frames can legitimately arrive for this event; keep collecting
	// so a delayed duplicate is observed rather than raced.
	time.Sleep(dupSettleWindow)

	if delta := perf.TunReadFramesTotal.Load() - readsBefore; delta != 1 {
		t.Fatalf("TUN read counter advanced by %d for one injected packet, want 1", delta)
	}

	packets := capture.snapshot()
	got := make([][]byte, 0, len(packets))
	for _, p := range packets {
		if p.src != netip.MustParseAddr(dupNodeAAddr) || p.dst != netip.MustParseAddr(dst) {
			t.Fatalf("delivered packet carries unexpected addresses %s -> %s", p.src, p.dst)
		}
		got = append(got, p.data)
	}
	want := [][]byte{payload}

	slices.SortFunc(got, bytes.Compare)
	slices.SortFunc(want, bytes.Compare)
	if !slices.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf("delivered frame multiset = %x, want exactly %x", got, want)
	}
}

// awaitRoute waits until n has a feasible route for prefix, and fails the test
// on a harness error or timeout. Reading router state goes through Dispatch so
// it never races the main loop.
func awaitRoute(t *testing.T, errs <-chan error, n *core.Nylon, prefix netip.Prefix) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-errs:
			t.Fatalf("harness error: %v", err)
		default:
		}
		ready := make(chan bool, 1)
		n.Dispatch(func() error {
			route, ok := n.RouterState.Routes[prefix]
			ready <- ok && route.Metric != state.INF
			return nil
		})
		select {
		case ok := <-ready:
			if ok {
				return
			}
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a route to %s", prefix)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
