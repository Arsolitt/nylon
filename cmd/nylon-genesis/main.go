// nylon-genesis generates AWG 2.0 obfuscation profiles for nylon meshes.
//
// This binary links the GPL-3.0 amnezigo parameter oracle, so distributed
// builds of it are conveyed under the GPL-3.0 (see LICENSE.GPL-3.0): anyone
// redistributing the binary must pass on the license and the corresponding
// source. The nylon daemon links only MIT device code and Apache-2.0 nylon
// code and carries no copyleft obligations.
package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	amnezigo "github.com/Arsolitt/amnezigo"
	"github.com/encodeous/nylon/internal/buildinfo"
	"github.com/encodeous/nylon/internal/logging"
	"github.com/encodeous/nylon/state"
	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"
)

const defaultPreset = "standard-1420"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		presetName string
		random     bool
		protocol   string
		mtu        int
		peerIds    string
		compat     bool
		outPath    string
		verbose    bool
	)

	cmd := &cobra.Command{
		Use:   "nylon-genesis",
		Short: "Generate an AWG 2.0 obfuscation profile fragment for a nylon central config",
		Long: `Generates a paste-ready YAML fragment: a mesh-wide shared obf profile
(S/H/J, uniform by construction) plus per-node obf blocks with sender-local
I1-I5 CPS sequences. Parameters come from the amnezigo oracle and are
validated before anything is emitted.

--compat emits the fixed vanilla-compat profile (H1..H4 pinned to the
WireGuard message types, no padding/junk/I) for the version-skew window
where plain-WireGuard peers must interoperate.`,
		Version:      buildinfo.Version,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(presetName, random, protocol, mtu, peerIds, compat, outPath, verbose)
		},
	}

	cmd.Flags().StringVar(&presetName, "preset", defaultPreset, "amnezigo preset name for the shared profile")
	cmd.Flags().BoolVar(&random, "random", false, "generate a random profile via amnezigo GenerateConfig instead of a preset")
	cmd.Flags().StringVar(&protocol, "protocol", amnezigo.ProtocolRandom, "protocol template for per-peer I1-I5 (quic|dns|dtls|stun|sip|rtp|random)")
	cmd.Flags().IntVar(&mtu, "mtu", 0, "underlay MTU clamping for I/junk sizes (0 = preset default)")
	cmd.Flags().StringVar(&peerIds, "peers", "", "comma-separated node ids to generate per-node I1-I5 for")
	cmd.Flags().BoolVar(&compat, "compat", false, "emit the fixed vanilla-compat profile (ignores preset/protocol)")
	cmd.Flags().StringVar(&outPath, "out", "", "write to FILE instead of stdout")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "log debug detail to stderr")

	return cmd
}

// peerFragment mirrors the CentralCfg/NodeCfg yaml shape so the output is
// paste-ready and unmarshals into state.CentralCfg without noise fields.
type peerFragment struct {
	Id  string               `yaml:"id"`
	Obf *state.ObfPeerParams `yaml:"obf,omitempty"`
}

type outputFragment struct {
	Obf     *state.ObfProfile `yaml:"obf,omitempty"`
	Routers []peerFragment    `yaml:"routers,omitempty"`
}

func run(presetName string, random bool, protocol string, mtu int, peerIds string, compat bool, outPath string, verbose bool) error {
	level, err := logging.Resolve("", verbose)
	if err != nil {
		return err
	}
	log, closer, err := logging.New(logging.Config{Component: "nylon-genesis", Level: level, JSON: false})
	if err != nil {
		return err
	}
	defer closer()
	if !slices.Contains(amnezigo.ListProtocols(), protocol) {
		return fmt.Errorf("unknown protocol %q; valid: %v", protocol, amnezigo.ListProtocols())
	}

	preset, err := amnezigo.GetPreset(presetName)
	if err != nil {
		return err
	}
	if mtu == 0 {
		mtu = preset.MTU
	}

	var peers []string
	for _, id := range strings.Split(peerIds, ",") {
		if id = strings.TrimSpace(id); id != "" {
			peers = append(peers, id)
		}
	}

	var profile state.ObfProfile
	var server amnezigo.ServerObfuscationConfig
	var peerParams map[string]*state.ObfPeerParams

	if compat {
		// The compat profile deliberately pins H1..H4 to the vanilla WG
		// message type ids; header-range validation is skipped because
		// excluding 1..4 is exactly what it would reject.
		profile = state.ObfProfile{
			H1: state.ObfHeaderRange{Min: 1, Max: 1},
			H2: state.ObfHeaderRange{Min: 2, Max: 2},
			H3: state.ObfHeaderRange{Min: 3, Max: 3},
			H4: state.ObfHeaderRange{Min: 4, Max: 4},
		}
		if err := profile.Validate(); err != nil {
			return fmt.Errorf("internal error: compat profile invalid: %w", err)
		}
	} else {
		if random {
			// One attempt draws the shared S/H/J (mesh-uniform by
			// construction) plus one I-set per peer; a draw can collide a
			// peer's I-packet length with a padded handshake size, so the
			// whole attempt is redrawn until it validates.
			profile, peerParams, err = generateRandomProfile(protocol, mtu, preset.S1, preset.Jc, peers)
			if err != nil {
				return err
			}
			log.Debug("validated profile")
			for _, id := range peers {
				p := peerParams[id]
				log.Debug("peer profile", "peer", id, "protocol", p.Protocol, "I1", p.I1, "I2", p.I2, "I3", p.I3, "I4", p.I4, "I5", p.I5)
			}
		} else {
			server = preset.ToServerObfuscation()
			profile = profileFromServer(server)

			if err := validateGenerated(&profile, server, peers, protocol); err != nil {
				return err
			}
			log.Debug("validated profile")

			peerParams = make(map[string]*state.ObfPeerParams, len(peers))
			for _, id := range peers {
				i1, i2, i3, i4, i5 := amnezigo.GenerateCPS(protocol, mtu, int(profile.S1), 0)
				params := &state.ObfPeerParams{
					Protocol: protocol,
					I1:       i1, I2: i2, I3: i3, I4: i4, I5: i5,
				}
				if err := validatePeerI(&profile, params); err != nil {
					return fmt.Errorf("peer %s: %w", id, err)
				}
				peerParams[id] = params
				log.Debug("peer profile", "peer", id, "protocol", params.Protocol, "I1", i1, "I2", i2, "I3", i3, "I4", i4, "I5", i5)
			}
		}
	}

	frag := outputFragment{Obf: &profile}
	for _, id := range peers {
		pf := peerFragment{Id: id}
		if peerParams != nil {
			pf.Obf = peerParams[id]
		}
		frag.Routers = append(frag.Routers, pf)
	}

	output := outPath
	if output == "" {
		output = "stdout"
	}
	log.Info("generated profile", "preset", presetName, "protocol", protocol, "mtu", mtu, "peers", len(peers), "output", output)

	data, err := yaml.Marshal(frag)
	if err != nil {
		return err
	}

	if outPath != "" {
		return os.WriteFile(outPath, data, 0o644)
	}
	_, err = os.Stdout.Write(data)
	return err
}

// randomProfileAttempts bounds the redraw loop in generateRandomProfile.
// A draw fails when a peer's I-packet length collides with one of the four
// padded handshake sizes (or, with several peers, when any one of them
// collides); the shared S/H/J and all peer I-sets are redrawn together, so
// attempts are independent. Protocol "random" with 5 peers was measured to
// fail ~45% of attempts, and the worst realistic profiles stay below that,
// so 64 attempts put the residual failure probability under 1e-20.
const randomProfileAttempts = 64

// generateRandomProfile draws an amnezigo random profile: one shared
// mesh-uniform S/H/J per attempt plus a distinct per-peer I1-I5 draw, the
// same shape the preset path emits. Every attempt is validated with the same
// validators as the preset path (validateGenerated, then validatePeerI per
// peer); the first fully valid attempt wins, and an invalid attempt is
// redrawn rather than surfaced, because a padded-size collision is an
// artifact of a single random draw and not a property of the inputs.
func generateRandomProfile(protocol string, mtu, s1, jc int, peers []string) (state.ObfProfile, map[string]*state.ObfPeerParams, error) {
	var lastErr error
	for range randomProfileAttempts {
		server := amnezigo.GenerateConfig(protocol, mtu, s1, jc).ServerObfuscationConfig
		profile := profileFromServer(server)
		if err := validateGenerated(&profile, server, peers, protocol); err != nil {
			lastErr = err
			continue
		}

		peerParams := make(map[string]*state.ObfPeerParams, len(peers))
		valid := true
		for _, id := range peers {
			i1, i2, i3, i4, i5 := amnezigo.GenerateCPS(protocol, mtu, int(profile.S1), 0)
			params := &state.ObfPeerParams{
				Protocol: protocol,
				I1:       i1, I2: i2, I3: i3, I4: i4, I5: i5,
			}
			if err := validatePeerI(&profile, params); err != nil {
				lastErr = fmt.Errorf("peer %s: %w", id, err)
				valid = false
				break
			}
			peerParams[id] = params
		}
		if !valid {
			continue
		}
		return profile, peerParams, nil
	}
	return state.ObfProfile{}, nil, fmt.Errorf("no valid random profile after %d attempts: %w", randomProfileAttempts, lastErr)
}

func profileFromServer(s amnezigo.ServerObfuscationConfig) state.ObfProfile {
	hr := func(h amnezigo.HeaderRange) state.ObfHeaderRange {
		return state.ObfHeaderRange{Min: h.Min, Max: h.Max}
	}
	return state.ObfProfile{
		Jc:   uint32(s.Jc),
		Jmin: uint32(s.Jmin),
		Jmax: uint32(s.Jmax),
		S1:   uint32(s.S1),
		S2:   uint32(s.S2),
		S3:   uint32(s.S3),
		S4:   uint32(s.S4),
		H1:   hr(s.H1),
		H2:   hr(s.H2),
		H3:   hr(s.H3),
		H4:   hr(s.H4),
	}
}

// validateGenerated runs the amnezigo validators over the shared profile:
// header-range viability plus the size-classification invariant for the
// S-padded handshake sizes and the junk range. (amnezigo's ValidateServerConfig
// additionally requires interface fields a profile fragment does not carry,
// so its obfuscation sub-checks are invoked directly instead.)
func validateGenerated(profile *state.ObfProfile, server amnezigo.ServerObfuscationConfig, peers []string, protocol string) error {
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("invalid obf profile: %w", err)
	}
	for i, h := range []amnezigo.HeaderRange{server.H1, server.H2, server.H3, server.H4} {
		if err := amnezigo.ValidateHeaderRange(h); err != nil {
			return fmt.Errorf("h%d: %w", i+1, err)
		}
	}
	if err := amnezigo.ValidatePacketSizes(
		int(profile.S1), int(profile.S2), int(profile.S3), int(profile.S4),
		nil, int(profile.Jmin), int(profile.Jmax),
	); err != nil {
		return fmt.Errorf("profile sizes: %w", err)
	}
	return nil
}

// validatePeerI re-checks the size-classification invariant with the freshly
// generated per-peer I lengths.
func validatePeerI(profile *state.ObfProfile, params *state.ObfPeerParams) error {
	iSizes := []int{
		amnezigo.CPSLength(params.I1),
		amnezigo.CPSLength(params.I2),
		amnezigo.CPSLength(params.I3),
		amnezigo.CPSLength(params.I4),
		amnezigo.CPSLength(params.I5),
	}
	return amnezigo.ValidatePacketSizes(
		int(profile.S1), int(profile.S2), int(profile.S3), int(profile.S4),
		iSizes, int(profile.Jmin), int(profile.Jmax),
	)
}
