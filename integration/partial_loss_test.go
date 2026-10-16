//go:build integration

package integration

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/encodeous/nylon/core"
	"github.com/encodeous/nylon/protocol"
	"github.com/encodeous/nylon/state"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestPartialLossReroutesViaTransit(t *testing.T) {
	defer goleak.VerifyNone(t)

	tunables := state.DefaultRouterTunables()
	tunables.ProbeDelay = 200 * time.Millisecond
	tunables.ProbeRecoveryDelay = 300 * time.Millisecond
	tunables.ProbeTimeout = 400 * time.Millisecond
	tunables.LinkDeadThreshold = 8 * time.Second
	tunables.GcDelay = 200 * time.Millisecond
	tunables.RouteUpdateDelay = 1 * time.Second
	tunables.HandshakeStaleDemote = 0

	vh := &VirtualHarness{}
	vh.Tunables = &tunables
	a1 := "192.168.1.1:1234"
	vh.NewNode("a", "10.0.0.1/32")
	b1 := "192.168.1.2:1234"
	vh.NewNode("b", "10.0.0.2/32")
	c1 := "192.168.1.3:1234"
	vh.NewNode("c", "10.0.0.3/32")
	vh.Central.Graph = []string{
		"a, b, c",
	}
	vh.Endpoints = map[string]state.NodeId{
		a1: "a",
		b1: "b",
		c1: "c",
	}
	ab := vh.AddLink(a1, b1).WithLatency(10*time.Millisecond, 0)
	ba := vh.AddLink(b1, a1).WithLatency(10*time.Millisecond, 0)
	vh.AddLink(a1, c1).WithLatency(20*time.Millisecond, 0)
	vh.AddLink(c1, a1).WithLatency(20*time.Millisecond, 0)
	vh.AddLink(b1, c1).WithLatency(20*time.Millisecond, 0)
	vh.AddLink(c1, b1).WithLatency(20*time.Millisecond, 0)

	errs := vh.Start()
	defer vh.Stop()

	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case err := <-errs:
			t.Error(err)
		case <-watchDone:
		}
	}()

	vn := vh.Net
	a := vh.Nylons[vh.IndexOf("a")].Load()
	b := vh.Nylons[vh.IndexOf("b")].Load()

	var mu sync.Mutex
	delivered := 0
	transit := 0

	const (
		baselineMarker byte = 111
		transitMarker  byte = 222
	)

	vn.TransitHandler = func(node state.NodeId, src, dst netip.Addr, data []byte) bool {
		if node == "c" && src.String() == "10.0.0.1" && dst.String() == "10.0.0.2" && data[0] == transitMarker {
			mu.Lock()
			transit++
			mu.Unlock()
		}
		return false
	}
	vn.SelfHandler = func(node state.NodeId, src, dst netip.Addr, data []byte) bool {
		if node == "b" && src.String() == "10.0.0.1" && dst.String() == "10.0.0.2" {
			mu.Lock()
			delivered++
			mu.Unlock()
		}
		return true
	}

	status := func(n *core.Nylon) *protocol.StatusResponse {
		return ipcCall(t, n, &protocol.IpcRequest{
			Request: &protocol.IpcRequest_Status{Status: &protocol.StatusRequest{}},
		}).GetStatus()
	}
	selectedNh := func(st *protocol.StatusResponse, prefix string) string {
		for _, r := range st.GetRoutes().GetSelected() {
			if r.GetPubRoute().GetSource().GetPrefix() == prefix {
				return r.GetNh()
			}
		}
		return ""
	}
	lossDeadEndpoint := func(st *protocol.StatusResponse, peer string) bool {
		for _, neigh := range st.GetNeighbours() {
			if neigh.GetPeerId() != peer {
				continue
			}
			for _, ep := range neigh.GetEndpoints() {
				if ep.GetActive() && ep.GetMetric() == 4294967295 {
					return true
				}
			}
		}
		return false
	}
	waitForRoute := func(n *core.Nylon, prefix, nh string, timeout time.Duration) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if selectedNh(status(n), prefix) == nh {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	sendMarkers := func(marker byte, count int) {
		for range count {
			select {
			case <-vh.Context.Done():
				return
			case <-time.After(100 * time.Millisecond):
				vn.Send("a", "10.0.0.1", "10.0.0.2", []byte{marker}, 64)
			}
		}
	}
	waitUntil := func(timeout time.Duration, cond func() bool) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return cond()
	}

	require.True(t, waitForRoute(a, "10.0.0.2/32", "b", 30*time.Second),
		"phase 1: route 10.0.0.2/32 did not converge to Nh b on a")

	mu.Lock()
	base := delivered
	mu.Unlock()
	sendMarkers(baselineMarker, 20)
	require.True(t, waitUntil(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered-base >= 18
	}), "phase 1: fewer than 18/20 baseline markers delivered via b")

	ab.SetPacketLoss(0.93)
	ba.SetPacketLoss(0.93)

	flipped := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		sa := status(a)
		sb := status(b)
		if selectedNh(sa, "10.0.0.2/32") == "c" &&
			selectedNh(sb, "10.0.0.1/32") == "c" &&
			lossDeadEndpoint(sa, "b") {
			flipped = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, flipped, "phase 2: routes did not flip to Nh c via transit within 25s")

	mu.Lock()
	base = delivered
	mu.Unlock()
	sendMarkers(transitMarker, 20)
	require.True(t, waitUntil(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered-base >= 18 && transit >= 1
	}), "phase 2: fewer than 18/20 markers delivered via c or none transited via c")

	ab.SetPacketLoss(0)
	ba.SetPacketLoss(0)
	require.True(t, waitForRoute(a, "10.0.0.2/32", "b", 20*time.Second),
		"phase 3: route 10.0.0.2/32 did not recover to Nh b on a")
}
