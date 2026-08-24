/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

// Golden classification vectors for Device.DeterminePacketTypeAndPadding.
//
// Vectors were generated on 2026-08-25 by executing the reference
// implementation amneziaawg-go v3.1.20260814 (commit 1b86b2a — the exact
// tree the AWG 2.0 port in polyamide/device was taken from). A one-off
// generator program (kept outside this repo) built an upstream
// device.Device on a ChannelTUN, configured knob state purely through the
// upstream UAPI socket protocol ("set=1" + s/h knob lines), called the
// upstream DeterminePacketTypeAndPadding over a deterministic input
// matrix, and printed the expectations below as ground truth.
//
// Matrix: three profiles (compat h1=1-1..h4=4-4; the fixed obf profile
// jc=5 jmin=250 jmax=800 s1=32 s2=28 s3=20 s4=16 h1=100-5000000
// h2=10000000-200000000 h3=400000000-800000000 h4=1000000000-2100000000;
// and all-defaults/empty). Per profile: per message class a packet with
// the class's ranged type word at the profile's padding offset at the
// exact expected size, one byte short, one byte long (handshake classes
// match on exact size; transport uses a size>=expectedSize gate, so
// over-long transport packets are valid), a large transport packet,
// pseudo-random garbage, and range-edge words. Regenerate whenever the
// port source changes.

import (
	"encoding/hex"
	"testing"

	"github.com/encodeous/nylon/polyamide/conn"
	"github.com/encodeous/nylon/polyamide/tun/tuntest"
)

// goldenVector is one classification input and the upstream reference
// output for it.
type goldenVector struct {
	name        string
	profile     string
	packet      string // hex-encoded packet passed to DeterminePacketTypeAndPadding
	wantMsgSize int
	wantMsgType uint32
	wantPadding uint32
}

// goldenProfileCfg maps a profile name to the UAPI knob lines that both
// the upstream generator and this test feed to IpcSet.
// 46 vectors across 3 profiles
var goldenProfileCfg = map[string]string{
	"compat": "h1=1-1\nh2=2-2\nh3=3-3\nh4=4-4\n",
	"obf":    "jc=5\njmin=250\njmax=800\ns1=32\ns2=28\ns3=20\ns4=16\nh1=100-5000000\nh2=10000000-200000000\nh3=400000000-800000000\nh4=1000000000-2100000000\n",
	"empty":  "",
}

var goldenClassificationVectors = []goldenVector{
	{name: "compat/initiation-exact", profile: "compat", packet: "01000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 148, wantMsgType: 1, wantPadding: 0},
	{name: "compat/initiation-short", profile: "compat", packet: "01000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "compat/initiation-long", profile: "compat", packet: "01000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "compat/response-exact", profile: "compat", packet: "02000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 92, wantMsgType: 2, wantPadding: 0},
	{name: "compat/response-short", profile: "compat", packet: "02000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "compat/response-long", profile: "compat", packet: "02000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "compat/cookie-exact", profile: "compat", packet: "03000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 64, wantMsgType: 3, wantPadding: 0},
	{name: "compat/cookie-short", profile: "compat", packet: "03000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "compat/cookie-long", profile: "compat", packet: "03000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "compat/transport-exact", profile: "compat", packet: "04000000abababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 0},
	{name: "compat/transport-short", profile: "compat", packet: "04000000ababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "compat/transport-long", profile: "compat", packet: "04000000ababababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 0},
	{name: "compat/transport-large", profile: "compat", packet: "04000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 0},
	{name: "compat/garbage-200", profile: "compat", packet: "dc622521052ebf9c59c1a5373052a5bd917303cc2a0db38b839fda8c66d096144c72207068c8c88c199b8a1983fa74b5d1311ba0a4a4a85aa03f9d95037f3121234c4d76e368bc8f9d1003f549aeba1c7613efa83fd4fa555a8ffc1c76f66d4132851611c6bc3cd826b551a90723db81a79f39f9e77e56bcb5214719441cdaeb6e27ff2a55ff37841575c0f9b83c294b92bde31c842f7c7755a8124a985e71d05886b479a43a90e3639941ed7dd2bc065f33b3afbb8f919d29665c3274ef2eab4ae92ed1ffa620fd", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/initiation-exact", profile: "obf", packet: "abababababababababababababababababababababababababababababababab87d61200abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 148, wantMsgType: 1, wantPadding: 32},
	{name: "obf/initiation-short", profile: "obf", packet: "abababababababababababababababababababababababababababababababab87d61200ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/initiation-long", profile: "obf", packet: "abababababababababababababababababababababababababababababababab87d61200ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/response-exact", profile: "obf", packet: "abababababababababababababababababababababababababababab15cd5b07abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 92, wantMsgType: 2, wantPadding: 28},
	{name: "obf/response-short", profile: "obf", packet: "abababababababababababababababababababababababababababab15cd5b07ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/response-long", profile: "obf", packet: "abababababababababababababababababababababababababababab15cd5b07ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/cookie-exact", profile: "obf", packet: "ababababababababababababababababababababe31a1d21abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 64, wantMsgType: 3, wantPadding: 20},
	{name: "obf/cookie-short", profile: "obf", packet: "ababababababababababababababababababababe31a1d21ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/cookie-long", profile: "obf", packet: "ababababababababababababababababababababe31a1d21ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/transport-exact", profile: "obf", packet: "ababababababababababababababababb1327976abababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 16},
	{name: "obf/transport-short", profile: "obf", packet: "ababababababababababababababababb1327976ababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/transport-long", profile: "obf", packet: "ababababababababababababababababb1327976ababababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 16},
	{name: "obf/transport-large", profile: "obf", packet: "ababababababababababababababababb1327976abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 16},
	{name: "obf/garbage-200", profile: "obf", packet: "dc622521052ebf9c59c1a5373052a5bd917303cc2a0db38b839fda8c66d096144c72207068c8c88c199b8a1983fa74b5d1311ba0a4a4a85aa03f9d95037f3121234c4d76e368bc8f9d1003f549aeba1c7613efa83fd4fa555a8ffc1c76f66d4132851611c6bc3cd826b551a90723db81a79f39f9e77e56bcb5214719441cdaeb6e27ff2a55ff37841575c0f9b83c294b92bde31c842f7c7755a8124a985e71d05886b479a43a90e3639941ed7dd2bc065f33b3afbb8f919d29665c3274ef2eab4ae92ed1ffa620fd", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/initiation-h1lo", profile: "obf", packet: "abababababababababababababababababababababababababababababababab64000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 148, wantMsgType: 1, wantPadding: 32},
	{name: "obf/initiation-h1lo-minus1", profile: "obf", packet: "abababababababababababababababababababababababababababababababab63000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "obf/transport-h4hi", profile: "obf", packet: "abababababababababababababababab00752b7dabababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 16},
	{name: "obf/transport-h4hi-plus1", profile: "obf", packet: "abababababababababababababababab01752b7dabababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/initiation-exact", profile: "empty", packet: "01000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 148, wantMsgType: 1, wantPadding: 0},
	{name: "empty/initiation-short", profile: "empty", packet: "01000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/initiation-long", profile: "empty", packet: "01000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/response-exact", profile: "empty", packet: "02000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 92, wantMsgType: 2, wantPadding: 0},
	{name: "empty/response-short", profile: "empty", packet: "02000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/response-long", profile: "empty", packet: "02000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/cookie-exact", profile: "empty", packet: "03000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 64, wantMsgType: 3, wantPadding: 0},
	{name: "empty/cookie-short", profile: "empty", packet: "03000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/cookie-long", profile: "empty", packet: "03000000ababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/transport-exact", profile: "empty", packet: "04000000abababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 0},
	{name: "empty/transport-short", profile: "empty", packet: "04000000ababababababababababababababababababababababababababab", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
	{name: "empty/transport-long", profile: "empty", packet: "04000000ababababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 0},
	{name: "empty/transport-large", profile: "empty", packet: "04000000abababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab", wantMsgSize: 32, wantMsgType: 4, wantPadding: 0},
	{name: "empty/garbage-200", profile: "empty", packet: "dc622521052ebf9c59c1a5373052a5bd917303cc2a0db38b839fda8c66d096144c72207068c8c88c199b8a1983fa74b5d1311ba0a4a4a85aa03f9d95037f3121234c4d76e368bc8f9d1003f549aeba1c7613efa83fd4fa555a8ffc1c76f66d4132851611c6bc3cd826b551a90723db81a79f39f9e77e56bcb5214719441cdaeb6e27ff2a55ff37841575c0f9b83c294b92bde31c842f7c7755a8124a985e71d05886b479a43a90e3639941ed7dd2bc065f33b3afbb8f919d29665c3274ef2eab4ae92ed1ffa620fd", wantMsgSize: 0, wantMsgType: 0, wantPadding: 0},
}

// TestGoldenClassificationUpstream1b86b2a replays every golden vector
// against this port: the device is configured via IpcSet with the same
// knob lines the upstream generator used, DeterminePacketTypeAndPadding
// is called with the same packet and a zeroed 4-byte type hash, and the
// classification triple must match the upstream expectation exactly.
func TestGoldenClassificationUpstream1b86b2a(t *testing.T) {
	for _, v := range goldenClassificationVectors {
		t.Run(v.name, func(t *testing.T) {
			cfg, ok := goldenProfileCfg[v.profile]
			if !ok {
				t.Fatalf("vector %q references unknown profile %q", v.name, v.profile)
			}
			packet, err := hex.DecodeString(v.packet)
			if err != nil {
				t.Fatalf("vector %q has invalid hex packet: %v", v.name, err)
			}

			dev := NewDevice(tuntest.NewChannelTUN().TUN(), conn.NewDefaultBind(), NewLogger(LogLevelError, "golden: "))
			defer dev.Close()
			if err := dev.IpcSet(cfg); err != nil {
				t.Fatalf("IpcSet(%q): %v", cfg, err)
			}

			msgSize, msgType, padding := dev.DeterminePacketTypeAndPadding(packet, make([]byte, 4))
			if msgSize != v.wantMsgSize || msgType != v.wantMsgType || padding != v.wantPadding {
				t.Errorf("classification = (%d, %d, %d), upstream want (%d, %d, %d)",
					msgSize, msgType, padding, v.wantMsgSize, v.wantMsgType, v.wantPadding)
			}
		})
	}
}
