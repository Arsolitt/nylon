package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	amnezigo "github.com/Arsolitt/amnezigo"
	"github.com/encodeous/nylon/state"
	"github.com/goccy/go-yaml"
)

// generate runs the generator into a temp file and unmarshals the output
// into state.CentralCfg, proving the fragment is paste-ready.
//
// Generation is driven by package-global crypto/rand, so the checks below
// are structural only: exact output bytes are never asserted.
func generate(t *testing.T, presetName string, random bool, protocol string, mtu int, peerIds string, compat bool) state.CentralCfg {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "out.yaml")
	if err := run(presetName, random, protocol, mtu, peerIds, compat, outPath, false); err != nil {
		t.Fatalf("run(preset=%q, random=%v, protocol=%q, mtu=%d, peers=%q, compat=%v) failed: %v",
			presetName, random, protocol, mtu, peerIds, compat, err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated output: %v", err)
	}
	var cfg state.CentralCfg
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("generated output does not unmarshal into state.CentralCfg: %v\noutput:\n%s", err, data)
	}
	return cfg
}

// validateProfile re-runs the amnezigo obfuscation sub-validators over a
// generated (non-compat) profile: per-H range viability plus the AWG 2.0
// size invariant for every peer's I-packet lengths. It also asserts the
// GenerateHeaderRanges invariant: generated ranges stay clear of the
// vanilla WG message type-ids 1..4.
func validateProfile(t *testing.T, profile *state.ObfProfile, peers []*state.ObfPeerParams) {
	t.Helper()
	if profile == nil {
		t.Fatal("central obf profile is nil")
	}
	for i, h := range [4]state.ObfHeaderRange{profile.H1, profile.H2, profile.H3, profile.H4} {
		if err := amnezigo.ValidateHeaderRange(amnezigo.HeaderRange{Min: h.Min, Max: h.Max}); err != nil {
			t.Errorf("h%d failed ValidateHeaderRange: %v", i+1, err)
		}
		if h.Min <= 4 {
			t.Errorf("h%d min (%d) must exceed the vanilla WG type-id window 1..4", i+1, h.Min)
		}
	}
	for _, p := range peers {
		iSizes := []int{
			amnezigo.CPSLength(p.I1),
			amnezigo.CPSLength(p.I2),
			amnezigo.CPSLength(p.I3),
			amnezigo.CPSLength(p.I4),
			amnezigo.CPSLength(p.I5),
		}
		if err := amnezigo.ValidatePacketSizes(
			int(profile.S1), int(profile.S2), int(profile.S3), int(profile.S4),
			iSizes, int(profile.Jmin), int(profile.Jmax),
		); err != nil {
			t.Errorf("packet sizes invalid for peer I-set %q: %v", p.I1, err)
		}
	}
}

func TestGeneratePresetProfileValidates(t *testing.T) {
	cfg := generate(t, "standard-1420", false, "quic", 1420, "node-a,node-b", false)

	if len(cfg.Routers) != 2 {
		t.Fatalf("expected exactly 2 routers, got %d", len(cfg.Routers))
	}
	for i, want := range []string{"node-a", "node-b"} {
		if got := string(cfg.Routers[i].Id); got != want {
			t.Errorf("routers[%d].Id = %q, want %q", i, got, want)
		}
	}

	var peers []*state.ObfPeerParams
	for i, r := range cfg.Routers {
		if r.Obf == nil {
			t.Fatalf("routers[%d].Obf is nil", i)
		}
		if r.Obf.Protocol != "quic" {
			t.Errorf("routers[%d].Obf.Protocol = %q, want %q", i, r.Obf.Protocol, "quic")
		}
		// Named protocol templates fill I1-I4; I5 is intentionally left
		// empty by the amnezigo oracle (see DNSTemplate et al.).
		for j, seq := range []string{r.Obf.I1, r.Obf.I2, r.Obf.I3, r.Obf.I4} {
			if seq == "" {
				t.Errorf("routers[%d].Obf.I%d is empty", i, j+1)
			}
		}
		peers = append(peers, r.Obf)
	}

	validateProfile(t, cfg.Obf, peers)
}

func TestGenerateCompatProfile(t *testing.T) {
	cfg := generate(t, defaultPreset, false, "quic", 0, "node-a", true)

	if cfg.Obf == nil {
		t.Fatal("central obf profile is nil")
	}
	want := [4]state.ObfHeaderRange{
		{Min: 1, Max: 1}, {Min: 2, Max: 2}, {Min: 3, Max: 3}, {Min: 4, Max: 4},
	}
	for i, h := range [4]state.ObfHeaderRange{cfg.Obf.H1, cfg.Obf.H2, cfg.Obf.H3, cfg.Obf.H4} {
		if h != want[i] {
			t.Errorf("h%d = {%d,%d}, want {%d,%d}", i+1, h.Min, h.Max, want[i].Min, want[i].Max)
		}
	}
	for name, v := range map[string]uint32{
		"jc": cfg.Obf.Jc, "jmin": cfg.Obf.Jmin, "jmax": cfg.Obf.Jmax,
		"s1": cfg.Obf.S1, "s2": cfg.Obf.S2, "s3": cfg.Obf.S3, "s4": cfg.Obf.S4,
	} {
		if v != 0 {
			t.Errorf("%s = %d, want 0 in compat profile", name, v)
		}
	}

	if len(cfg.Routers) != 1 {
		t.Fatalf("expected exactly 1 router, got %d", len(cfg.Routers))
	}
	if cfg.Routers[0].Obf != nil {
		t.Errorf("compat routers[0].Obf = %+v, want nil (no I blocks)", cfg.Routers[0].Obf)
	}
}

func TestGenerateRandomProfileValidates(t *testing.T) {
	cfg := generate(t, "standard-1420", true, "dns", 1420, "x", false)

	if len(cfg.Routers) != 1 {
		t.Fatalf("expected exactly 1 router, got %d", len(cfg.Routers))
	}
	if cfg.Routers[0].Obf == nil {
		t.Fatal("routers[0].Obf is nil")
	}
	validateProfile(t, cfg.Obf, []*state.ObfPeerParams{cfg.Routers[0].Obf})
}

// TestGenerateRandomProfileFivePeersValidates drives the random path with
// enough peers that the redraw loop in generateRandomProfile is
// load-bearing. With protocol "random" every peer misses the padded
// handshake sizes independently (measured: ~11% collision per peer at
// mtu 1420), so a single raw attempt with 5 peers is invalid ~45% of the
// time: without the redraw this test fails roughly every other run, while
// the 64-attempt budget leaves a ~0.45^64 residual failure probability.
func TestGenerateRandomProfileFivePeersValidates(t *testing.T) {
	ids := []string{"n1", "n2", "n3", "n4", "n5"}
	cfg := generate(t, "standard-1420", true, amnezigo.ProtocolRandom, 1420, strings.Join(ids, ","), false)

	if len(cfg.Routers) != len(ids) {
		t.Fatalf("expected exactly %d routers, got %d", len(ids), len(cfg.Routers))
	}
	var peers []*state.ObfPeerParams
	for i, want := range ids {
		if got := string(cfg.Routers[i].Id); got != want {
			t.Errorf("routers[%d].Id = %q, want %q", i, got, want)
		}
		obf := cfg.Routers[i].Obf
		if obf == nil {
			t.Fatalf("routers[%d].Obf is nil", i)
		}
		if obf.Protocol != amnezigo.ProtocolRandom {
			t.Errorf("routers[%d].Obf.Protocol = %q, want %q", i, obf.Protocol, amnezigo.ProtocolRandom)
		}
		// Random mode fills every interval; named templates leave I5 empty.
		for j, seq := range []string{obf.I1, obf.I2, obf.I3, obf.I4, obf.I5} {
			if seq == "" {
				t.Errorf("routers[%d].Obf.I%d is empty", i, j+1)
			}
		}
		peers = append(peers, obf)
	}
	validateProfile(t, cfg.Obf, peers)

	// Each peer must keep its own draw: reusing one I-set for every peer
	// would still satisfy the validators but violates the per-sender
	// obfuscation contract this generator exists to produce.
	shared := true
	for _, p := range peers[1:] {
		if *p != *peers[0] {
			shared = false
			break
		}
	}
	if shared {
		t.Error("all peers share one I-set; per-peer I1-I5 draws collapsed into a single draw")
	}
}

func TestGenerateUnknownProtocolFails(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.yaml")
	err := run("standard-1420", false, "smtp", 1420, "node-a", false, outPath, false)
	if err == nil {
		t.Fatal("expected an error for unknown protocol, got nil")
	}
	if _, statErr := os.Stat(outPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("no output file should be created on error; stat err = %v", statErr)
	}
}
