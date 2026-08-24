package device

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/encodeous/nylon/polyamide/tun/tuntest"
)

// obfTestProfile returns device-level UAPI lines with non-trivial S/H values
// whose padded handshake sizes are pairwise distinct (S+148, S+92, S+64,
// S+32) and whose H ranges sit far outside the vanilla WG type-id window.
func obfTestProfile() []string {
	return []string{
		"jc", "2",
		"jmin", "50",
		"jmax", "90",
		"s1", "30",
		"s2", "25",
		"s3", "15",
		"s4", "10",
		"h1", "100-200",
		"h2", "300-400",
		"h3", "500-600",
		"h4", "700-800",
	}
}

func TestUAPISetGetObfKnobs(t *testing.T) {
	goroutineLeakCheck(t)
	dev := randDevice(t)
	defer dev.Close()

	if err := dev.IpcSet(uapiCfg(obfTestProfile()...)); err != nil {
		t.Fatalf("failed to set obf knobs: %v", err)
	}
	if err := dev.IpcSet(uapiCfg("i1", "<r 12>")); err != nil {
		t.Fatalf("failed to set i1: %v", err)
	}

	var buf bytes.Buffer
	if err := dev.IpcGetOperation(&buf); err != nil {
		t.Fatalf("get operation failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"jc=2\n", "jmin=50\n", "jmax=90\n",
		"s1=30\n", "s2=25\n", "s3=15\n", "s4=10\n",
		"h1=100-200\n", "h2=300-400\n", "h3=500-600\n", "h4=700-800\n",
		"i1=<r 12>\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("UAPI get output missing %q; got:\n%s", want, out)
		}
	}
}

func TestUAPISetOverlappingHeadersRejected(t *testing.T) {
	goroutineLeakCheck(t)
	dev := randDevice(t)
	defer dev.Close()

	err := dev.IpcSet(uapiCfg(
		"h1", "100-300",
		"h2", "200-400",
	))
	if err == nil {
		t.Fatal("expected overlapping H ranges to be rejected")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected overlap error, got: %v", err)
	}
}

// TestUAPISetRejects3xKnobs asserts the AWG 3.x-only knobs are stubbed off so
// no external UAPI writer can activate a non-2.0 wire format (design §2.3).
func TestUAPISetRejects3xKnobs(t *testing.T) {
	goroutineLeakCheck(t)
	dev := randDevice(t)
	defer dev.Close()

	stubbed := []string{
		"header_protection_key",
		"content_padding_addition",
		"random_trailers",
		"disable_cookies",
		"rekey_after_time",
		"rekey_timeout",
		"reject_after_time",
		"keepalive_timeout",
		"max_handshake_attempts",
	}
	for _, knob := range stubbed {
		if err := dev.IpcSet(uapiCfg(knob, "1")); err == nil {
			t.Errorf("knob %q was accepted; expected 3.x-only rejection", knob)
		} else if !strings.Contains(err.Error(), "3.x-only") {
			t.Errorf("knob %q rejected with unclear error: %v", knob, err)
		}
	}
}

// TestPairObfHandshake runs a full ping exchange with identical non-trivial
// S/H on both endpoints.
func TestPairObfHandshake(t *testing.T) {
	goroutineLeakCheck(t)
	pair := genTestPair(t, true)
	profile := uapiCfg(obfTestProfile()...)
	for i := range pair {
		if err := pair[i].dev.IpcSet(profile); err != nil {
			t.Fatalf("failed to set obf profile on device %d: %v", i, err)
		}
		// The TUN reader computes its read offset per iteration and may be
		// blocked inside Read on the pre-profile offset. Push one sacrificial
		// packet (unrouted destination, dropped by traffic control) so the
		// reader completes an iteration AFTER the profile landed; the next
		// packet is then read with the obfuscated layout. Channel FIFO makes
		// this deterministic.
		pair[i].tun.Outbound <- tuntest.Ping(pair[i].ip, netip.MustParseAddr("9.9.9.9"))
	}
	t.Run("ping 1.0.0.1", func(t *testing.T) {
		pair.Send(t, Ping, nil)
	})
	t.Run("ping 1.0.0.2", func(t *testing.T) {
		pair.Send(t, Pong, nil)
	})
}

// TestPairObfHeaderMismatch asserts mismatched H ranges classify the peer's
// handshakes as unknown type and drop them (design §2.4).
func TestPairObfHeaderMismatch(t *testing.T) {
	goroutineLeakCheck(t)
	pair := genTestPair(t, true)
	if err := pair[0].dev.IpcSet(uapiCfg(
		"h1", "100-200", "h2", "300-400", "h3", "500-600", "h4", "700-800",
	)); err != nil {
		t.Fatalf("failed to set H on device 0: %v", err)
	}
	if err := pair[1].dev.IpcSet(uapiCfg(
		"h1", "1000-1100", "h2", "1200-1300", "h3", "1400-1500", "h4", "1600-1700",
	)); err != nil {
		t.Fatalf("failed to set H on device 1: %v", err)
	}

	p0, p1 := pair[0], pair[1]
	p1.tun.Outbound <- tuntest.Ping(p1.ip, p0.ip)

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case msgRecv := <-p0.tun.Inbound:
		t.Fatalf("packet transited despite mismatched H ranges: %v", msgRecv)
	case <-timer.C:
		// expected: classification drop
	}
}
