//go:build awg_ref_interop

// Package awgref proves wire-format compatibility between nylon's ported
// AWG 2.0 stack (polyamide/device, ported from amneziawg-go commit 1b86b2a)
// and the upstream reference implementation
// github.com/amnezia-vpn/amneziawg-go/v3 v3.1.20260814 — the exact tag of
// that commit and therefore the lineage the port was audited against.
//
// How to run:
//
//	go test -tags awg_ref_interop ./interop/awgref/ -v -count=1
//
// The test is fully in-process: a nylon device and a reference device, each
// backed by its own tuntest.ChannelTUN and a real UDP socket on 127.0.0.1
// (nylon 127.0.0.1:57310, reference 127.0.0.1:57311), are configured through
// their respective UAPI IpcSet implementations with identical device-level
// obfuscation knobs (jc/jmin/jmax/s1-s4 and h1-h4 min-max ranges — the same
// lines core.initWireGuard synthesizes from a CentralCfg obf profile, which
// must precede the first public_key line in both parsers).
//
// What it proves: with obfuscation active on both ends, the two independent
// implementations complete a Noise handshake and exchange byte-identical
// IPv4 packets in both directions. That exercises — and pins — the whole
// 2.0 wire format across implementations: obfuscated message-type selection
// from the h1-h4 ranges, S-padding and its removal, junk-packet tolerance
// (jc=5), and the transport-data path, as specified in docs/design.md §2.4.
//
// Retargeting for a future AWG 3.0 reference: bump the amneziawg-go version
// in go.mod (and re-audit docs/design.md §2.3 verdicts for that tree), then
// extend knobLines() below with whatever device-level knobs 3.0 adds (its
// UAPI grows e.g. header_protection_key, content_padding_addition,
// random_trailers — all currently stubbed off in nylon's uapi.go). Both
// devices are fed from the same profile string, so one edit retargets both
// sides.
package awgref

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"

	refconn "github.com/amnezia-vpn/amneziawg-go/v3/conn"
	refdev "github.com/amnezia-vpn/amneziawg-go/v3/device"
	reftuntest "github.com/amnezia-vpn/amneziawg-go/v3/tun/tuntest"

	"github.com/encodeous/nylon/polyamide/conn"
	"github.com/encodeous/nylon/polyamide/device"
	"github.com/encodeous/nylon/polyamide/tun/tuntest"
)

const (
	nylonPort = 57310 // UDP port of the nylon device's bind
	refPort   = 57311 // UDP port of the reference device's bind

	nylonIP = "10.0.0.1" // tunnel address of the nylon side
	refIP   = "10.0.0.2" // tunnel address of the reference side

	handshakeTimeout = 10 * time.Second
	dataTimeout      = 5 * time.Second
	keepaliveSecs    = 5
)

// knobLines are the device-level obfuscation UAPI lines shared by both
// implementations. They match the fixed test profile of
// e2e/testdata/obf-profile.yaml and must appear before any public_key line:
// both IpcSet parsers switch from device configuration to peer configuration
// at the first public_key they encounter.
const knobLines = "jc=5\n" +
	"jmin=250\n" +
	"jmax=800\n" +
	"s1=32\n" +
	"s2=28\n" +
	"s3=20\n" +
	"s4=16\n" +
	"h1=100-5000000\n" +
	"h2=10000000-200000000\n" +
	"h3=400000000-800000000\n" +
	"h4=1000000000-2100000000\n"

// ipcDevice is the UAPI surface both implementations expose.
type ipcDevice interface {
	IpcSet(string) error
	IpcGet() (string, error)
}

type keyPair struct {
	priv [32]byte
	pub  [32]byte
}

// genKeyPair generates a curve25519 keypair the same way both stacks derive
// public keys internally: X25519 clamps the private scalar, so the hex we
// hand to either IpcSet parser yields the same static public key.
func genKeyPair(t *testing.T) keyPair {
	t.Helper()
	var kp keyPair
	if _, err := rand.Read(kp.priv[:]); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pub, err := curve25519.X25519(kp.priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	copy(kp.pub[:], pub)
	return kp
}

// uapiCfg mirrors the helper in polyamide/device/device_test.go: alternating
// key/value strings joined into an IpcSet payload. It exists to keep editors
// and humans from inserting whitespace that IpcSet would silently misparse.
func uapiCfg(cfg ...string) string {
	if len(cfg)%2 != 0 {
		panic("odd number of args to uapiCfg")
	}
	var b strings.Builder
	for i, s := range cfg {
		b.WriteString(s)
		if i%2 == 0 {
			b.WriteByte('=')
		} else {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// waitForHandshake polls the device's UAPI get output until the peer reports
// a completed handshake (last_handshake_time_sec > 0).
func waitForHandshake(t *testing.T, d ipcDevice, name string) {
	t.Helper()
	deadline := time.Now().Add(handshakeTimeout)
	for {
		out, err := d.IpcGet()
		if err != nil {
			t.Fatalf("%s: IpcGet: %v", name, err)
		}
		for _, line := range strings.Split(out, "\n") {
			value, ok := strings.CutPrefix(line, "last_handshake_time_sec=")
			if !ok {
				continue
			}
			secs, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				t.Fatalf("%s: bad last_handshake_time_sec %q: %v", name, value, err)
			}
			if secs > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no handshake within %v", name, handshakeTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// expectPacket asserts that want arrives byte-identical on the channel within
// dataTimeout.
func expectPacket(t *testing.T, inbound <-chan []byte, want []byte, direction string) {
	t.Helper()
	timer := time.NewTimer(dataTimeout)
	defer timer.Stop()
	select {
	case got := <-inbound:
		if !bytes.Equal(want, got) {
			t.Fatalf("%s: packet corrupted on transit\nwant: %x\ngot:  %x", direction, want, got)
		}
	case <-timer.C:
		t.Fatalf("%s: packet did not transit within %v", direction, dataTimeout)
	}
}

// TestNylonAwgInteropWithReference runs a nylon device against the upstream
// amneziawg-go reference over real UDP sockets and asserts a full handshake
// plus bidirectional byte-identical data flow under an active obfuscation
// profile.
func TestNylonAwgInteropWithReference(t *testing.T) {
	nylonAddr := netip.MustParseAddr(nylonIP)
	refAddr := netip.MustParseAddr(refIP)

	nylonKey := genKeyPair(t)
	refKey := genKeyPair(t)

	nylonLevel, refLevel := device.LogLevelError, refdev.LogLevelError
	if testing.Verbose() {
		nylonLevel, refLevel = device.LogLevelVerbose, refdev.LogLevelVerbose
	}

	nylonTun := tuntest.NewChannelTUN()
	nylonDev := device.NewDevice(nylonTun.TUN(), conn.NewDefaultBind(), device.NewLogger(nylonLevel, "nylon: "))
	refTun := reftuntest.NewChannelTUN()
	refDev := refdev.NewDevice(refTun.TUN(), refconn.NewDefaultBind(), refdev.NewLogger(refLevel, "awgref: "))
	t.Cleanup(nylonDev.Close)
	t.Cleanup(refDev.Close)

	nylonCfg := knobLines + uapiCfg(
		"private_key", hex.EncodeToString(nylonKey.priv[:]),
		"listen_port", strconv.Itoa(nylonPort),
		"replace_peers", "true",
		"public_key", hex.EncodeToString(refKey.pub[:]),
		"replace_allowed_ips", "true",
		"allowed_ip", refAddr.String()+"/32",
		"endpoint", fmt.Sprintf("127.0.0.1:%d", refPort),
		"persistent_keepalive_interval", strconv.Itoa(keepaliveSecs),
	)
	refCfg := knobLines + uapiCfg(
		"private_key", hex.EncodeToString(refKey.priv[:]),
		"listen_port", strconv.Itoa(refPort),
		"replace_peers", "true",
		"public_key", hex.EncodeToString(nylonKey.pub[:]),
		"replace_allowed_ips", "true",
		"allowed_ip", nylonAddr.String()+"/32",
		"endpoint", fmt.Sprintf("127.0.0.1:%d", nylonPort),
		"persistent_keepalive_interval", strconv.Itoa(keepaliveSecs),
	)

	if err := nylonDev.IpcSet(nylonCfg); err != nil {
		t.Fatalf("failed to configure nylon device: %v", err)
	}
	if err := refDev.IpcSet(refCfg); err != nil {
		t.Fatalf("failed to configure reference device: %v", err)
	}
	if err := nylonDev.Up(); err != nil {
		t.Fatalf("failed to bring up nylon device: %v", err)
	}
	if err := refDev.Up(); err != nil {
		t.Fatalf("failed to bring up reference device: %v", err)
	}

	// Both stacks start RoutineReadFromTUN inside NewDevice, before any
	// obfuscation profile is configured. The reader computes its read offset
	// (message header + S4 prefix) once per loop iteration and then blocks in
	// Read, so a reader parked before IpcSet would consume the first packet
	// with the vanilla (prefix-less) layout and emit an unclassifiable
	// near-vanilla transport message. Push one unrouted sacrificial packet
	// (destination outside every allowed_ip) into each side so the stale read
	// iteration completes and is dropped by the routing lookup; every
	// subsequent packet is then read with the obfuscated layout. Same trick as
	// polyamide/device/obf_test.go.
	unrouted := netip.MustParseAddr("9.9.9.9")
	nylonTun.Outbound <- tuntest.Ping(unrouted, nylonAddr)
	refTun.Outbound <- reftuntest.Ping(unrouted, refAddr)

	// A sacrificial outbound packet makes the nylon side initiate the
	// handshake immediately instead of waiting for the first keepalive.
	nylonTun.Outbound <- tuntest.Ping(refAddr, nylonAddr)

	waitForHandshake(t, nylonDev, "nylon")
	waitForHandshake(t, refDev, "awgref")
	t.Log("handshake complete between nylon and amneziawg-go reference")

	// Data plane, nylon -> reference.
	ping := tuntest.Ping(refAddr, nylonAddr)
	nylonTun.Outbound <- ping
	expectPacket(t, refTun.Inbound, ping, "nylon->reference")

	// Data plane, reference -> nylon.
	pong := tuntest.Ping(nylonAddr, refAddr)
	refTun.Outbound <- pong
	expectPacket(t, nylonTun.Inbound, pong, "reference->nylon")
}
