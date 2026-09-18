//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/encodeous/nylon/state"
)

// These tests reproduce the nylon0 drain bottleneck, which surfaces as growth
// of /sys/class/net/nylon0/statistics/tx_dropped under bulk traffic.
//
// A bulk TCP flow (iperf3) is routed through the TUN device of a CPU-limited
// node. When the userspace daemon stops draining the per-queue kernel ring
// (depth = tx_queue_len, 500 by default) the kernel drops packets it cannot
// hand to the daemon, and tx_dropped grows while `tc -s qdisc` still reports
// zero qdisc drops. The counter is therefore the observable under test: each
// test bounds its growth across the measurement window.
//
// TestTunDropReproduction1CPU runs the incident's shape (one CPU, eight TCP
// streams); TestTunDropFree4CPU gives the daemon four CPUs and twelve streams
// and asserts the same tight bound.

// tunDropDuration is the iperf3 generation window shared by both variants.
const tunDropDuration = 15

// maxTunDropDelta is the floor of the drop bound in the plan's predicate.
const maxTunDropDelta = 50

// minMeasuredBitsPerSecond guards the measurement itself. A run that carries
// almost no traffic would otherwise satisfy the drop bound trivially (no
// packets, no drops). These containers carry ~3 Gbit/s through the TUN, so this
// floor only trips on a datapath that is broken or stalled.
const minMeasuredBitsPerSecond = 1e8 // 100 Mbit/s

// iperf3Sum is the subset of the `iperf3 --json` report these tests assert on
// and log (iperf 3.20).
type iperf3Sum struct {
	SumSent struct {
		Bytes         int64   `json:"bytes"`
		BitsPerSecond float64 `json:"bits_per_second"`
		Retransmits   int64   `json:"retransmits"`
	} `json:"sum_sent"`
	SumReceived struct {
		Bytes         int64   `json:"bytes"`
		BitsPerSecond float64 `json:"bits_per_second"`
	} `json:"sum_received"`
}

type iperf3Result struct {
	End   iperf3Sum `json:"end"`
	Error string    `json:"error"`
}

// TestTunDropReproduction1CPU pushes eight TCP streams through a one-CPU node
// and bounds the kernel-side TUN transmit drops over the run.
func TestTunDropReproduction1CPU(t *testing.T) {
	t.Parallel()
	runTunDropScenario(t, 1, 8)
}

// TestTunDropFree4CPU repeats the scenario with four CPUs and twelve streams.
func TestTunDropFree4CPU(t *testing.T) {
	t.Parallel()
	runTunDropScenario(t, 4, 12)
}

// runTunDropScenario starts a two-router topology with the given per-container
// CPU limit, generates bulk TCP traffic through nylon0, and asserts that the
// sender's kernel transmit drops stay within the plan's bound.
func runTunDropScenario(t *testing.T, cpus float64, parallel int) {
	t.Helper()

	h := NewHarness(t)

	node1Key := state.GenerateKey()
	node2Key := state.GenerateKey()

	node1IP := GetIP(h.Subnet, 10)
	node2IP := GetIP(h.Subnet, 11)

	const node1NylonIP = "10.0.0.1"
	const node2NylonIP = "10.0.0.2"

	configDir := h.SetupTestDir()

	// node2 carries the real endpoint so node1 can resolve and reach it.
	central := state.CentralCfg{
		Routers: []state.RouterCfg{
			SimpleRouter("node1", node1Key.Pubkey(), node1NylonIP, ""),
			SimpleRouter("node2", node2Key.Pubkey(), node2NylonIP, node2IP),
		},
		Graph: []string{
			"node1, node2",
		},
		Timestamp: time.Now().UnixNano(),
	}

	centralPath := h.WriteConfig(configDir, "central.yaml", central)
	node1Path := h.WriteConfig(configDir, "node1.yaml", SimpleLocal("node1", node1Key))
	node2Path := h.WriteConfig(configDir, "node2.yaml", SimpleLocal("node2", node2Key))

	t.Logf("Starting 2-node topology: cpus=%g iperf3_parallel=%d duration=%ds", cpus, parallel, tunDropDuration)
	h.StartNodes(
		NodeSpec{Name: "node1", IP: node1IP, CentralConfigPath: centralPath, NodeConfigPath: node1Path, CPUs: cpus},
		NodeSpec{Name: "node2", IP: node2IP, CentralConfigPath: centralPath, NodeConfigPath: node2Path, CPUs: cpus},
	)

	// Wait for convergence: with a two-node graph each node announces its own
	// /32, so each peer installs the other's nylon address as a route.
	t.Log("Waiting for convergence...")
	h.WaitForMatch("node1", `installing new route.*prefix=10\.0\.0\.2`)
	h.WaitForMatch("node2", `installing new route.*prefix=10\.0\.0\.1`)

	// The datapath must carry traffic before the measurement window opens; the
	// ping also completes the first handshake.
	waitForNylonPing(t, h, "node1", node2NylonIP)

	txdBefore := mustTunTxDropped(t, h, "node1")
	node2TxdBefore, node2BeforeErr := tunTxDropped(h, "node2")
	t.Logf("tx_dropped before: node1=%d node2=%d (node2 read err=%v)", txdBefore, node2TxdBefore, node2BeforeErr)
	logTunCounters(t, h, "node1", "before")

	res := runIperf3(t, h, "node2", "node1", node2NylonIP, parallel, tunDropDuration)

	// A drop bound is only meaningful when the run actually carried traffic.
	if got := res.End.SumReceived.BitsPerSecond; got < minMeasuredBitsPerSecond {
		h.PrintLogs("node1")
		h.PrintLogs("node2")
		t.Fatalf("measurement invalid: iperf3 delivered only %.0f bits/s (floor %.0f bits/s, sent %.0f bits/s) - the datapath did not carry the load, so the drop bound below would be meaningless",
			got, minMeasuredBitsPerSecond, res.End.SumSent.BitsPerSecond)
	}

	txdAfter := mustTunTxDropped(t, h, "node1")
	node2TxdAfter, node2AfterErr := tunTxDropped(h, "node2")
	delta := txdAfter - txdBefore

	t.Logf("tx_dropped after: node1=%d node2=%d (node2 read err=%v)", txdAfter, node2TxdAfter, node2AfterErr)
	t.Logf("tx_dropped delta: node1=%d node2=%d", delta, node2TxdAfter-node2TxdBefore)
	logTunCounters(t, h, "node1", "after")

	// Plan Step 0.2 predicate, verbatim:
	//   txd_after - txd_before <= max(50, (txd_after-txd_before)/10)
	// The right-hand side only exceeds 50 for deltas above 500, so for any
	// meaningful measurement this reduces to "drops <= 50".
	limit := int64(maxTunDropDelta)
	if tenth := delta / 10; tenth > limit {
		limit = tenth
	}
	if delta > limit {
		h.PrintLogs("node1")
		h.PrintLogs("node2")
		t.Fatalf(
			"nylon0 kernel tx_dropped grew by %d over a %ds iperf3 run (limit %d): the daemon did not drain the TUN ring. sum_sent=%.0f bits/s sum_received=%.0f bits/s retransmits=%d",
			delta, tunDropDuration, limit, res.End.SumSent.BitsPerSecond, res.End.SumReceived.BitsPerSecond, res.End.SumSent.Retransmits,
		)
	}
	t.Logf("tx_dropped delta %d within limit %d (sum_sent=%.0f bits/s sum_received=%.0f bits/s retransmits=%d)",
		delta, limit, res.End.SumSent.BitsPerSecond, res.End.SumReceived.BitsPerSecond, res.End.SumSent.Retransmits)
}

// runIperf3 serves on serverNode and generates from clientNode towards target,
// returning the client's summary. Every raw number is logged by the caller's
// test log through this function.
func runIperf3(t *testing.T, h *Harness, serverNode, clientNode, target string, parallel int, seconds int) iperf3Result {
	t.Helper()

	if stdout, stderr, err := h.Exec(serverNode, []string{"iperf3", "-s", "-D", "-1"}); err != nil {
		h.PrintLogs(serverNode)
		t.Fatalf("failed to start iperf3 server on %s: %v\nStdout: %s\nStderr: %s", serverNode, err, stdout, stderr)
	}
	h.WaitForTCPListener(t, serverNode, 5201)
	t.Logf("iperf3 server listening on %s:5201", serverNode)

	cmd := []string{"iperf3", "-c", target, "-t", strconv.Itoa(seconds), "-P", strconv.Itoa(parallel), "--json"}
	stdout, stderr, err := h.Exec(clientNode, cmd)
	if err != nil {
		h.PrintLogs(clientNode)
		h.PrintLogs(serverNode)
		t.Fatalf("iperf3 client failed on %s (%v): %v\nStdout: %s\nStderr: %s", clientNode, cmd, err, stdout, stderr)
	}
	t.Logf("iperf3 raw output (%v on %s):\n%s", cmd, clientNode, stdout)

	raw := stdout
	if start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); start >= 0 && end > start {
		raw = raw[start : end+1]
	}
	var res iperf3Result
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("failed to parse iperf3 json: %v\nRaw output: %s", err, stdout)
	}
	if res.Error != "" {
		t.Fatalf("iperf3 reported an error: %s", res.Error)
	}

	t.Logf("iperf3 sums: sum_sent.bits_per_second=%.0f sum_sent.bytes=%d sum_sent.retransmits=%d sum_received.bits_per_second=%.0f sum_received.bytes=%d",
		res.End.SumSent.BitsPerSecond, res.End.SumSent.Bytes, res.End.SumSent.Retransmits,
		res.End.SumReceived.BitsPerSecond, res.End.SumReceived.Bytes)
	return res
}

// waitForNylonPing waits until from can reach target through the nylon TUN.
//
// The reply is checked in the output, not only through the returned error:
// `h.Exec` reads the exit status from a separate docker inspect, so a command
// that failed can still surface as a nil error. A ping that lost every packet
// would otherwise let the test start measuring on a blackholed datapath.
func waitForNylonPing(t *testing.T, h *Harness, from, target string) {
	t.Helper()
	deadline := time.Now().Add(WaitTimeout)
	for {
		stdout, stderr, err := h.Exec(from, []string{"ping", "-c", "1", "-W", "2", target})
		replied := strings.Contains(stdout, "bytes from") || strings.Contains(stdout, "1 received")
		if err == nil && replied {
			t.Logf("ping %s from %s succeeded: %s", target, from, strings.TrimSpace(stdout))
			return
		}
		t.Logf("ping %s from %s not reachable yet (err=%v replied=%t): %s%s",
			target, from, err, replied, strings.TrimSpace(stdout), strings.TrimSpace(stderr))
		if time.Now().After(deadline) {
			h.PrintLogs(from)
			t.Fatalf("timed out waiting for %s to reach %s", from, target)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// tunTxDropped reads the kernel-side transmit drop counter of the nylon TUN.
func tunTxDropped(h *Harness, node string) (int64, error) {
	stdout, stderr, err := h.Exec(node, []string{"cat", "/sys/class/net/nylon0/statistics/tx_dropped"})
	if err != nil {
		return 0, fmt.Errorf("reading tx_dropped on %s: %w (stdout=%q stderr=%q)", node, err, stdout, stderr)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing tx_dropped on %s: %w (stdout=%q)", node, err, stdout)
	}
	return v, nil
}

// mustTunTxDropped reads the counter for a node whose measurement is asserted.
func mustTunTxDropped(t *testing.T, h *Harness, node string) int64 {
	t.Helper()
	v, err := tunTxDropped(h, node)
	if err != nil {
		h.PrintLogs(node)
		t.Fatalf("%v", err)
	}
	return v
}

// logTunCounters dumps the interface counters and qdisc state so that a
// failing run carries its own diagnosis.
func logTunCounters(t *testing.T, h *Harness, node, phase string) {
	t.Helper()
	for _, cmd := range [][]string{
		{"sh", "-c", "tc -s qdisc show dev nylon0"},
		{"sh", "-c", "ip -s link show nylon0"},
		{"sh", "-c", "grep nylon0 /proc/net/dev"},
	} {
		stdout, stderr, err := h.Exec(node, cmd)
		t.Logf("[%s] %s on %s (err=%v):\n%s%s", phase, strings.Join(cmd, " "), node, err, stdout, stderr)
	}
}
