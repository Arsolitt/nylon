# Nylon Fork Design — polyamide-awg, Gossip Membership, Dynamic Prefixes, nylon-lb

Status: **design document** — no production code changes in this round.
Date: 2026-08-22 · Base: `c3f872d98aad9d474927c108c74a94de571f4bfc` (`v0.4.5-4-gc3f872d`) · Branch: `fork/design`

Continuation of the "Nylon + AmneziaWG self-healing underlay for k3s" design (Obsidian note 2026-08-16).
Changes vs that note: dynamic prefix contract is `prefixes.d/*.json` (not `*.conf`); nylon-lb is fully
designed (§6); the AWG wire-format target is locked to **2.0**; the polyamide↔amneziawg-go diff-audit
has been executed and its outcome is recorded here (§2, Appendix A).

---

## 1. Overview & goals

Replace the Cilium Geneve overlay for k3s with a self-healing WireGuard-based mesh underlay built on
nylon. The mesh provides: any-to-any routed connectivity with Babel-style distance-vector convergence
(RTT-aware metrics), per-node prefix announcement with health-gated withdrawal, and — as a fork —
AmneziaWG 2.0 traffic obfuscation to make the underlay's handshakes indistinguishable from a chosen
ordinary protocol.

Three fork parts:

1. **polyamide-awg** — port of the AmneziaWG 2.0 device layer into nylon's vendored `polyamide/device/`
   tree (§2).
2. **Gossip membership + dynamic prefixes** — decentralized membership replacing central-config
   distribution (§3), plus a local `prefixes.d/*.json` contract for runtime prefix announcement by
   external writers such as k8s sidecars and the nylon-lb agent (§4).
3. **k8s integration** — DaemonSets, systemd units, and Cilium native-routing configuration that bind
   the mesh to k3s (§5); **nylon-lb**, a LoadBalancer implementation announcing VIP /32s into the mesh
   (§6).

**Accepted trade-off — hop-by-hop encryption.** WireGuard encrypts per peer-pair; transit routers
decrypt and re-encrypt. Mesh-internal traffic is visible to every transit node. Accepted: all mesh
nodes are under single administrative control.

**Fork strategy.** nylon's `polyamide` subtree is vendored in-tree (no submodule; module path
`github.com/encodeous/nylon/polyamide/...`). The fork maintains its changes as a **patchset over
pinned upstream tags** — never a merge-forever branch. Concretely:

- The vendored base is pin-identified: polyamide's wireguard-go base is exactly
  `ecfc5a8d54462e18e13c72173e2623d16d8e25a0` (2026-05-22, merged via commits `9a360bb`/`2a93dbf`).
- Every fork release is produced by re-applying the fork patchset (nylon mods + AWG port) onto the
  pinned base; upstream re-syncs regenerate the port against the new tag rather than merging history.
- **Version-skew wire compatibility is a release acceptance criterion**: a mesh mid-upgrade (nodes
  running the previous plain-WireGuard polyamide next to polyamide-awg nodes) must keep forwarding.
  Mechanism and constraints in §2.4.

---

## 2. polyamide-awg — AmneziaWG 2.0 port

### 2.1 Target: AWG 2.0 wire format only (user decision)

- **In:** the full 2.0 parameter surface — `Jc/Jmin/Jmax` (junk packets), `S1–S4` (message paddings),
  `H1–H4` (message-header ranges), `I1–I5` (custom signature packets, CPS tag grammar
  `<b 0x…>/<r n>/<rd n>/<rc n>/<t>`).
- **Out:** everything AWG 3.x adds on top — header protection (`HeaderProtectionKey`/HPK),
  `ContentPaddingAddition`, `random_trailers`, `disable_cookies`, and the timings overrides.
  These knobs are rejected at nylon's UAPI boundary (§2.3) and never written by nylon's config
  synthesis.
- amnezigo (`~/projects/amnezigo`, GPL-3.0) remains the **parameter oracle**, unchanged: nylon asks
  it for valid parameter sets; it never interprets AWG internals itself.

### 2.2 Source revision and HPK-unset ≡ 2.0 evidence

Port source: **amneziawg-go `1b86b2ae0e493e7ea93f8c1a0f0cb6735b1551f1`** (tag `v3.1.20260814`,
2026-08-13). Module `github.com/amnezia-vpn/amneziawg-go/v3`; `device/` LICENSE is the inherited
wireguard-go MIT (WireGuard LLC).

At this revision the 3.x header-protection paths are cleanly **gated by the zero value of the HPK**,
so the 2.0 behavior is the default when the key is never set:

- `device/noise-protocol.go:647–656` — `HeaderProtectionCipher` returns `(nil, nil)` when
  `device.headerProtection.key.IsZero()`; only a non-zero key constructs the chacha20 header cipher.
- `device/send.go:165–171` (handshake initiation; same pattern at `:222` response, `:250+` cookie,
  `:638` transport field) — `cip, err := …HeaderProtectionCipher(…)`; the XOR is applied only under
  `if cip != nil`.
- `device/receive.go:144–166` — with HPK unset, `typeHash` stays zeroed (`:150–152`), so
  `applyHash` is the identity and classification reduces to the pure S/H machinery:
  `device.DeterminePacketTypeAndPadding` (`device/receive.go:604–658`) matches each message class by
  exact `padding(S) + MessageSize` and `H` range containment.
- `device/uapi.go:852–859` — the `S%d must be more then %d to use headerProtection` constraint is
  enforced only `if !d.headerProtectionKey.IsZero()` (`HeaderCipherNonceSize = 12`,
  `device/noise-types.go:23`).
- README.md:66 — *"If there is no value specified (for any param), AWG treats it as 0"*; the
  `Header protection [AWG 3+]` README section documents HPK as the sole opt-in knob.

**Decision (fallback rule applied):** port HEAD `1b86b2a` and **stub HPK off at the UAPI boundary**
(one small patch in `uapi.go`, rejecting `header_protection_key` — see §2.3) instead of surgically
excising the gated branches from `send.go`/`receive.go`. Rationale: the branches are inert when the
key is zero; excision would touch exactly the hunks that must stay verbatim for future re-applies.
Recorded fallback if PoC stage 2 (§7) finds HEAD's 2.0 encoding incompatible with reference AWG 2.0
peers: rebase the port onto **`v0.2.19`** = `1cc94272ca8e9e223a5fe76382f5880f09d3c12d`, the newest
tag whose `device/` contains no HPK references (and the last to carry `magic-header.go`).

### 2.3 Port plan (audit outcome)

Per-file verdicts for porting amneziawg-go `device/` into `polyamide/device/`. "AWG Δ" and "nylon Δ"
are line counts vs the common wireguard-go base `ecfc5a8d` (raw `git diff --no-index --stat` tables
in Appendix A).

| File(s) | AWG Δ | nylon Δ | Verdict | Notes |
|---|---|---|---|---|
| `obf.go, obf_bytes.go, obf_data.go, obf_datasize.go, obf_datastring.go, obf_rand.go, obf_randchars.go, obf_randdigits.go, obf_timestamp.go` | +548 (new) | — | **port-verbatim** | 2.0 CPS engine; stdlib-only imports; no module-path churn |
| `noise-types.go` | +108 | 0 | **port-verbatim** | `UintRange` (H ranges), `HeaderCipherKey` types (compile, stay unused while HPK is never set) |
| `constants.go` | +1 | 0 | port-verbatim | `DefaultUdpWindow = 500` |
| `cookie.go` | 3 | 0 | port-verbatim | `CreateReply` takes `msgType` (H3-selected cookie type) |
| `noise-protocol.go` | 138 | 6 | **port-adapted** | AWG: `JunkPackets()` `:632–645`, `HeaderProtectionCipher` `:647–656`, header-type selection `:199/:375`; nylon Δ is module-path trivia |
| `device.go` | 68 | 55 | **port-adapted** | both edit `Device` struct (nylon `:63..`, AWG params `:89–115`) and `NewDevice` (`:282..` vs `:285..`): merge param structs into nylon's struct |
| `peer.go` | 15 | 168 | **port-adapted** | AWG: `Peer` struct fields, `NewPeer` `+2`; nylon reworked `NewPeer` `:97..` and `Stop` `:264..` for traffic control — hunks adjacent, merge by hand |
| `send.go` | 239 | 241 | **port-adapted** (heaviest) | overlap: `SendHandshakeInitiation/Response` (nylon `:115../:205..`, AWG `:98../:152..` — I-packets `:139–147`, junk `:147`, S-padding+trailer `:149–176`), `RoutineSequentialSender` (nylon `:511..`, AWG `:507..`); AWG-only: `calculatePaddingSize` rewrite `:431..`, `randomTrailer` `:554/:565`, transport header pick `:606` |
| `receive.go` | 162 | 126 | **port-adapted** | overlap: `RoutineHandshake` (nylon `:339..`, AWG `:276../:347../:383..`), `RoutineSequentialReceiver` (nylon `:461..`, AWG `:462../:503../:534..`); AWG-only: classification block `:132..`→`:139–166`, `DeterminePacketTypeAndPadding` `:598–658` |
| `timers.go` | 151 | 13 | port-adapted, hunk-level | AWG Δ is mostly 3.x timings plumbing (`timings` struct, `:231..`) — **exclude** those hunks; keep AWG handshake/keepalive hooks required by 2.0 (region `:169..`); nylon Δ at `:191` sits inside it |
| `uapi.go` | 439 | 67 | **port-adapted + stub-off patch** | overlap: `IpcGetOperation` (nylon `:104..`, AWG `:70..` + print-only-when-set `:143–155`), `handleDeviceLine` (nylon `:202..`, AWG knob parsing `:235..`→`+343–549`); AWG-only tail `ipcSetDevice` `:791–877` |
| `sticky_linux.go`, `sticky_default.go`, `tun.go`, `pools.go`, `logger.go`, `mobilequirks.go`, `keypair.go`, tests | ≤4 each (module-path) | 34/—/16/16/4/6/2 | keep polyamide versions | no AWG content beyond import-path renames |
| `status.go`, `traffic_control.go`, `traffic_manip.go`, `traffic_manip_test.go` | — | +519 (new) | keep polyamide versions | nylon-local: status, traffic shaping/manipulation |

**Stub-off patch (mandatory, small):** in `uapi.go` `handleDeviceLine`, reject the 3.x knobs —
`header_protection_key`, `content_padding_addition`, `random_trailers`, `disable_cookies`,
`rekey_after_time`, `rekey_timeout`, `reject_after_time`, `keepalive_timeout`,
`max_handshake_attempts` — with a clear error, so no external UAPI writer can activate a non-2.0
wire format. nylon's own synthesis writes only `jc/jmin/jmax/s1–s4/h1–h4/i1–i5`.

**UAPI surface exposed by nylon after the port (2.0 set):** `jc jmin jmax s1 s2 s3 s4 h1 h2 h3 h4
i1 i2 i3 i4 i5` plus the standard WireGuard knobs. Full knob inventory of amneziawg-go HEAD in
Appendix A.3.

### 2.4 Wire compatibility and version skew

Mechanics (from the ported code, not documentation claims):

- **S/H must agree between handshake endpoints.** The sender picks the 4-byte message type from its
  H range (`noise-protocol.go:199/:375`, `send.go:250/:606`) and pads by its S value; the receiver
  classifies with its own S/H (`receive.go:604–658`). Mismatch = packets classified as
  `MessageUnknownType` and dropped.
- **I/J are sender-local.** Junk and signature packets carry no state; the receiver ignores them
  (README: no need to specify on both sides).
- **"Obfuscation off" is not vanilla WireGuard.** With every param unset, `PickOne()` over a zero
  range yields message type 0 — AWG-default nodes speak type-0 handshakes to each other and are
  byte-incompatible with vanilla WG. Interop with plain-WireGuard peers requires a
  **vanilla-compat profile**: `H1=1, H2=2, H3=3, H4=4` (single-value ranges), `S1–S4=0`, `Jc=0`,
  no I-packets.
- **Rolling upgrade** therefore works like this: during a version-skew window the mesh genesis uses
  the vanilla-compat profile (old plain-polyamide nodes interop with polyamide-awg nodes); once all
  nodes run polyamide-awg, the profile is switched to the obfuscated one. The switch itself changes
  S/H live (atomic loads per message), so in-flight handshakes can fail for one rekey window —
  schedule it as a maintenance action, converging within a keepalive/rekey cycle. Mixed-profile
  meshes (some pairs compat, some obfuscated) are not representable with device-level S/H; full-mesh
  uniformity is assumed (§3 genesis distributes one shared S/H/J profile).
- **Release acceptance criterion:** pcap-verified interop matrix {vanilla polyamide ↔ polyamide-awg
  with compat profile; polyamide-awg ↔ polyamide-awg with obf profile; polyamide-awg ↔ reference
  AWG 2.0 peer (amneziawg-go `v0.2.19` or awg 1.5)} — exercised in PoC stages 1–2 (§7).

### 2.5 Mandatory small patches

- **TUN MTU knob** — `LocalCfg.MTU *uint16` (new field; nil = today's behavior,
  `device.DefaultMTU = 1420`, `polyamide/device/tun.go:14`, fallback at `device/device.go:296–301`).
  The knob feeds amnezigo at genesis/generation time, which clamps I-packet sizes to
  `maxISize = MTU − reserve(49) − handshakeSize(149) − S1` (`amnezigo/cps.go:14–15,50–52`) and
  enforces `Jmax < MTU`. Rationale: I1–I5 and junk packets exceeding the underlay MTU fragment —
  fragmentation is DPI-visible and explicitly warned about in the amneziawg-go README.
- **sd_notify watchdog hooks** (`Type=notify` + `WatchdogSec`) — required by R4, **out of scope this
  round**.

### 2.6 amnezigo consumption (genesis profile)

amnezigo is consumed **as a library** by the genesis tooling:

- `GetPreset(name)` (`presets.go:184`) — presets: `standard-1420`, `home-balanced`,
  `lan-conservative`, `low-overhead`, `mobile-aggressive`, `stealth-paranoid`, `test-minimal`.
- Protocol templates (`protocols.go:9–14`): `quic`, `dns`, `dtls`, `stun`, `sip`, `rtp` (+ `random`
  default) — drive per-peer CPS generation.
- CPS builder and validators: `cps.go` (`buildAndValidateCPS`, `maxISize` formula),
  `validation.go` — `ValidatePacketSizes` (`:77`; S-pair, I-vs-padded and junk-range collision
  classes), `ValidateHeaderRange` (`:192`), `ValidateServerConfig` (`:207`, severity-coded
  findings). Genesis output = shared S/H/J profile + per-peer I1–I5 CPS + validation verdict;
  per-peer I1–I5 travel inside the member record (§3).

### 2.7 License posture

Verified: amneziawg-go `device/` is MIT (inherited wireguard-go LICENSE, WireGuard LLC); amnezigo is
GPL-3.0; nylon is Apache-2.0. The fork keeps each file under its source license. Because the
GPL-3.0 oracle is linked into the same genesis tooling distribution as fork code, **combined
binaries are internal-only**; the nylon daemon itself links only MIT device code plus Apache-2.0
nylon code, but the conservative posture is applied fleet-wide: no public distribution of fork
binaries without a license review.

---

## 3. Gossip membership (design; not implemented this round)

### 3.1 Member record

```go
type ObfParams struct {
    Protocol string // amnezigo template: quic|dns|dtls|stun|sip|rtp
    I1, I2, I3, I4, I5 string // CPS tag sequences (per-peer)
}

type MemberRecord struct {
    Schema    uint8              // = 1
    Id        state.NodeId
    Epoch     uint64
    Version   uint64
    WGPub     state.NyPublicKey
    SignPub   ed25519.PublicKey
    Addresses []netip.Addr
    Endpoints []string
    Prefixes  []netip.Prefix
    Obf       ObfParams
    Signature []byte // ed25519 over the canonical (deterministic) encoding of the fields above
}
```

- `Prefixes` is the **announced list only**. Metric and health policy stay node-local; remote
  metrics arrive via Babel updates, exactly as today.
- Canonical encoding: fixed field order, fixed-width scalars, length-prefixed arrays, addresses and
  prefixes in binary form — deterministic, no map iteration, no JSON ambiguity.
- Signature covers everything except itself; `SignPub` is immutable within an `Epoch` (a record
  violating it is dropped and alert-logged).
- Merge order: `Epoch` → `Version` → canonical-hash tiebreak. Records are validated at merge/join;
  hard limit **2 KB** serialized (measured representative record: **979 B** canonical binary /
  **1,223 B** compact JSON with 10 endpoints, 4 prefixes, 5×100 B CPS strings, 2 addresses —
  Appendix B).

### 3.2 Interfaces (transport-agnostic)

```go
type RecordStore interface {
    Get(id NodeId) *MemberRecord
    Apply(rec MemberRecord) error // validates sig, size, epoch rules
    Snapshot() []MemberRecord
    Subscribe() <-chan MembershipEvent
}

type MembershipTransport interface {
    Join(introducers []string) error
    Leave() error
    BroadcastDigest()  // notify peers of local change
    TriggerPushPull()  // TCP full-state exchange
    Events() <-chan TransportEvent
}
```

### 3.3 v1 transport: hashicorp/memberlist

- SWIM probing on a **separate UDP port** from the data plane.
- `NodeMeta` ≤ 256 B carries only a ~24 B digest `{Schema, Epoch, Version, Hash}`.
- Full records exchange via `LocalState`/`MergeRemoteState` push-pull every **5 s** (mesh size
  assumption 3–10 nodes).
- `GetBroadcasts` = digest-notify only (no record fragmentation over the gossip plane).

### 3.4 Genesis and join

`node.yaml` gains a genesis block: `{key, id, sign-key, shared S/H/J profile, mesh-PSK, introducer
endpoints, admin keys}`. Join flow: contact introducer → join-snapshot (full `RecordStore` state) →
AWG handshakes with all members (full mesh default).

### 3.5 Config distribution

**`.nybundle` distribution is CANCELLED** (decision from the prior session). The distribution
poller machinery (`core/nylon_distribution.go`, whose apply entry is
`ApplyCentralConfig` at `:88`) is kept only as a genesis/debug fallback. In steady state, membership
feeds `ApplyCentralConfig` by synthesizing a `state.CentralCfg` from the `RecordStore` snapshot —
the same live-apply path used today (`core/nylon_apply.go:21`; flow:
`normalizeCentralConfig` → `reconcileRouterState` → `reconcileAdvertisedPrefixes` →
`SyncApplicationState`).

### 3.6 Node removal and rotation

- **Node removal v1:** manual re-genesis (documented runbook: stop node, regenerate genesis without
  it, roll nodes). Signed tombstones deferred.
- **S/H/J rotation v1:** full re-genesis (same runbook). Deferred: epoch-bumped in-band rotation.

---

## 4. Dynamic prefix contract — `prefixes.d` JSON

Replaces the earlier `*.conf` design. This contract is nylon's **k8s-free edge**: it works
pre-k8s with any writer (sidecar, nylon-lb agent, scripts).

### 4.1 Knob and writers

- New `LocalCfg` field `dynamic_prefixes_dir string` (empty = disabled; recommended
  `/etc/nylon/prefixes.d`).
- Files: `*.json`, **one writer per file** — k8s sidecar writes `10-podcidr.json`, nylon-lb agent
  writes `20-nylon-lb.json`. Writers must write tmp + rename (atomic).
- Directory hardening (R5): root-owned `0755` directory, root-owned `0644` files; only root/hostPath
  writers.

### 4.2 Schema v1

Field-for-field mirror of the three health-config structs in `state/prefix_health.go`
(`StaticPrefixHealth` `:31`, `PingPrefixHealth` `:59`, `HTTPPrefixHealth` `:202`; wrapper
`:311`), keyed by the `PrefixHealthConfig` implementation:

```json
{
  "version": 1,
  "prefixes": [
    {"type": "static", "prefix": "10.42.1.0/24", "metric": 0},
    {"type": "ping", "prefix": "10.1.0.0/24", "addr": "10.1.0.8",
     "delay": "10s", "max_failures": 3, "bind_if": "", "metric": 5},
    {"type": "http", "prefix": "10.2.0.0/24", "url": "http://example.com/healthz",
     "delay": "15s", "metric": 5}
  ]
}
```

| JSON field | Go field (struct) | Type |
|---|---|---|
| `type` | discriminator → `PrefixHealthWrapper.PrefixHealth` | `static` \| `ping` \| `http` |
| `prefix` | `.Prefix` (all three) | `netip.Prefix` |
| `metric` (static) | `StaticPrefixHealth.Metric` | `uint32` |
| `addr` | `PingPrefixHealth.Addr` | `netip.Addr` |
| `max_failures` | `PingPrefixHealth.MaxFailures` | `*int` |
| `delay` | `PingPrefixHealth.Delay` / `HTTPPrefixHealth.Delay` | `*time.Duration` (Go duration string) |
| `bind_if` | `PingPrefixHealth.BindIf` | `string` |
| `metric` (ping/http) | `.Metric` | `*uint32` |
| `url` | `HTTPPrefixHealth.URL` | `string` |

`metric` is optional (0 / nil defaults); durations are Go duration strings (`10s`, `1m30s`).

### 4.3 Merge semantics — one choke point

Injection happens inside `ApplyCentralConfig` (`core/nylon_apply.go:21`), **after**
`normalizeCentralConfig` (`:22–25`) and **before** the `reflect.DeepEqual` noop check (`:29`):

1. `candidate` = normalized central config (validation runs on central content only).
2. Append the parsed dynamic entries (held in a new field, planned `Nylon.dynamicPrefixes`) to the
   local node's `Prefixes []state.PrefixHealthWrapper` entry in `candidate`.
3. The noop check now sees dynamic state — steady state with unchanged files = `ApplyNoop`.
4. `reconcileRouterState` (`:36`) → `reconcileAdvertisedPrefixes` (`:39`) start/stop health monitors
   for the merged set and update `RouterState.Advertised` (`state/routing.go:45–46`).
5. Commit `n.CentralCfg = *candidate` (`:40`).

All three callers inherit the injection: the distribution poller
(`core/nylon_distribution.go:88`), IPC reload (`core/ipc_handler.go:469`), and the new watcher
(§4.5). Central updates never clobber dynamic state because injection is re-applied from
`Nylon.dynamicPrefixes` on every apply, and the committed `CentralCfg` carries the merged view.

### 4.4 Conflict and validation rules (fail-closed, deterministic)

- Entry whose prefix already exists in the central config for this node → **reject entry**, error
  log.
- Duplicate prefix across files → first file in **lexicographic filename order** wins; later entry
  rejected with a warning.
- Malformed file or unknown `version` → **skip the whole file** with an error; other files still
  apply.
- Prefixes parsed with `netip.ParsePrefix` + `Masked()` (canonical host bits rejected).
- Files considered: `*.json` only, sorted by filename; union of valid files appended in that order.

### 4.5 Watcher

fsnotify (`github.com/fsnotify/fsnotify` — a new dependency; nylon has none today; lands with the
watcher implementation, not this round) on `dynamic_prefixes_dir`:

- Initial scan at startup (before first `ApplyCentralConfig`).
- Any event → 250 ms debounce → **full rescan** → rebuild set → `ApplyCentralConfig`.
- Delete or emptied file = withdraw: `reconcileAdvertisedPrefixes` stops the monitors and retracts
  the prefixes via the existing Babel machinery (`RouterState.Advertised` maintenance).

---

## 5. k8s integration

Carried over from the 2026-08-16 note, updated to the JSON contract:

- **Pod-CIDR sidecar** — hostNetwork DaemonSet reads `CiliumNode.spec.ipam.podCIDRs`
  (fallback `Node.spec.podCIDR`) and writes `10-podcidr.json` into `/etc/nylon/prefixes.d` (hostPath).
- **Boot order** — nylon systemd unit orders `Before=kubelet`; sequence: nylon → k3s → sidecar →
  announce. The mesh is up (and prefixes from the previous run re-announced) before the kubelet
  schedules pods.
- **Cilium** — `routingMode: native`, encryption **off**, `ipv4-native-routing-cidr` = the static
  aggregate of all node pod CIDRs (design-time constant), masquerade egress-only.
- **kubelet** — `--node-ip=<nylon mesh address>` per node.
- **MTU alignment** — pod MTU must account for WG + AWG overhead: nylon TUN MTU (§2.5 knob) minus
  underlay headers; Cilium `mtu` set consistently; the same MTU feeds amnezigo's `maxISize` clamp.
- **systemd unit** — `Restart=always`, `RestartSec=1`, `StartLimitIntervalSec=0`; `Type=notify` +
  `WatchdogSec` once §2.5(b) lands; `MemoryMax` bound.
- **Procedures (R4)** — worker drain runbook (cordon, wait for Babel re-convergence, stop nylon);
  server maintenance window for control-plane nodes (etcd/apiserver depend on the mesh).

---

## 6. nylon-lb — LoadBalancer over the mesh (design only, this round)

### 6.1 Concept

A MetalLB-BGP-mode analogue with **Babel as the distribution protocol**. A Service VIP is a `/32`
from a services slice of the mesh supernet, announced through the §4 JSON contract by an agent on
selected nodes.

### 6.2 Components

- **Controller** (Deployment):
  - watches Services `type=LoadBalancer`; runs IPAM over the pool;
  - writes desired `VIP → node-set` state into a ConfigMap `nylon-lb-state`;
  - sets `status.loadBalancer.ingress` and appends the VIP to `spec.externalIPs`, so kube-proxy /
    Cilium program the VIP→endpoints DNAT — **no custom data plane**;
  - finalizer `nylon-lb/allocator` prevents VIP leaks on Service deletion.
- **Agent** (DaemonSet, nodes labeled `nylon-lb=true`):
  - reconciles `nylon-lb-state` → writes/removes `20-nylon-lb.json`;
  - binds/releases the VIP /32 on `lo` via netlink (local termination, health probes).

### 6.3 IPAM

Pool prefix + `mode` live in a ConfigMap; allocations persist in `nylon-lb-state` as a
`service-key → VIP` map. The pool is carved from the mesh supernet **at design time**, disjoint
from node and pod ranges.

### 6.4 Modes

- **`anycast` (default)** — every eligible node announces the same /32. Babel's RTT-based metric
  steers each mesh source to its nearest announcer; node death auto-withdraws the announcement.
  Caveat: mid-flow reroute breaks long-lived TCP (same failure class as MetalLB non-ECMP;
  acceptable for ingress-style traffic).
- **`single`** — one announcer chosen by the controller (Service annotation), predictable path.

### 6.5 Prerequisites to validate in PoC (stage 3b)

- Same prefix advertised by ≥ 2 nodes, and nylon reprogramming WG `AllowedIPs` when Babel selection
  changes. Babel source-change/feasibility semantics exist upstream
  (`core/router_test.go:963` `TestRouter_SelectedNeighbourUnfeasibleSourceChangeIsUnselected`;
  `state/validation.go:123–124` explicitly permits duplicate prefixes across routers for anycast) —
  **end-to-end confirmation required** before building on it.

### 6.6 Reachability

VIPs are reachable from all mesh participants and from anything that routes the mesh supernet.
Public exposure requires external NAT/routing — explicitly **out of scope**.

---

## 7. PoC plan

1. **Vanilla nylon** — 3 VMs, netem break A↔B: convergence time, throughput, long-lived TCP
   survival across link failure/recovery.
2. **polyamide-awg** — pcap: handshake indistinguishable from the mimicked protocol (target from
   `Obf.Protocol` template); interop matrix of §2.4 (compat profile vs vanilla; obf profile
   node-to-node; reference AWG 2.0 peer); re-benchmark throughput (R1 perf gate).
3. **k3s + Cilium native + `prefixes.d` JSON** — break the direct link between two nodes: verify
   pod-to-pod, apiserver, etcd, and in-cluster DB reachability across the healed path.
   **3b.** two-node same-/32 announce → anycast check for §6 (§6.5 prerequisites).
4. **Soak** — random degradation schedule (link loss, latency injection, node restarts); also
   exercises Babel RTT de-preference.

---

## 8. Risk register

| # | Risk | Mitigation |
|---|---|---|
| R1 | AWG port costs throughput (padding/junk/extra copies) | perf gate first-class in PoC stage 2; abort criterion if mesh throughput drops beyond agreed budget vs vanilla |
| R2 | Upstream fragility (wireguard-go/amneziawg-go drift breaks the port) | patchset-over-tags strategy (§1); pinned bases; e2e + netem + pcap CI in the fork; version-skew wire compat as release criterion (§2.4) |
| R3 | k8s IPAM changes `podCIDR` on node recreation | sidecar rewrites `10-podcidr.json`; withdraw/re-announce handled by existing health/Babel machinery; acceptance covered in PoC stage 3 |
| R4 | nylon is a per-node SPOF | systemd `Restart=always` + `StartLimitIntervalSec=0` + watchdog (§2.5b); drain/maintenance procedures (§5) |
| R5 | `prefixes.d` is an unauthenticated local control surface | directory root-owned `0755`, files root-owned `0644`; only root/hostPath writers; fail-closed parsing (§4.4) |

---

## Appendix A — audit evidence

### A.1 Revisions

| Repo | Commit | Notes |
|---|---|---|
| nylon (this fork) | `c3f872d98aad9d474927c108c74a94de571f4bfc` | `v0.4.5-4-gc3f872d`, branch `fork/design` |
| polyamide base (wireguard-go) | `ecfc5a8d54462e18e13c72173e2623d16d8e25a0` | vendored via merges `9a360bb`/`2a93dbf`; module `github.com/encodeous/nylon/polyamide` |
| amneziawg-go (port source) | `1b86b2ae0e493e7ea93f8c1a0f0cb6735b1551f1` | tag `v3.1.20260814`, 2026-08-13; module `github.com/amnezia-vpn/amneziawg-go/v3`; device/ MIT |
| amneziawg-go fallback | `1cc94272ca8e9e223a5fe76382f5880f09d3c12d` | tag `v0.2.19` — newest tag with zero HPK references in `device/` (last with `magic-header.go`) |
| amnezigo | `~/projects/amnezigo` (GPL-3.0) | oracle; `cps.go`, `presets.go`, `protocols.go`, `validation.go` |

### A.2 HPK gating (amneziawg-go `1b86b2a`, quotes verbatim)

`device/noise-protocol.go:647–656`:

```go
func (device *Device) HeaderProtectionCipher(salt []byte) (*chacha20.Cipher, error) {
	device.headerProtection.RLock()
	defer device.headerProtection.RUnlock()

	if device.headerProtection.key.IsZero() {
		return nil, nil
	}

	return chacha20.NewUnauthenticatedCipher(device.headerProtection.key[:], salt)
}
```

`device/send.go:165–171` (handshake initiation; same gate at `:222`, `:275`, `:638`):

```go
	cip, err := peer.device.HeaderProtectionCipher(crypt[:HeaderCipherNonceSize])
	if err != nil {
		return err
	}
	if cip != nil {
		cip.XORKeyStream(packet, packet)
	}
```

`device/receive.go:150–157`:

```go
		typeHash := typeHashBuf[:]
		clear(typeHash)
		if cip != nil {
			cip.XORKeyStream(typeHash, typeHash)
		}

		// get message padding and type based on information from S1-S4 and H1-H4
		msgSize, msgType, padding := device.DeterminePacketTypeAndPadding(packet, typeHash)
```

`device/uapi.go:852–859`:

```go
	if !d.headerProtectionKey.IsZero() {
		paddings := []uint32{d.paddings.init, d.paddings.response, d.paddings.cookie, d.paddings.transport}
		for i, padding := range paddings {
			if padding < HeaderCipherNonceSize {
				return fmt.Errorf("S%d must be more then %d to use headerProtection", i, HeaderCipherNonceSize)
			}
		}
	}
```

`README.md:66`: `> If there is no value specified (for any param), AWG treats it as 0`
(`HeaderCipherNonceSize = 12`, `device/noise-types.go:23`).

### A.3 Knob classification (amneziawg-go HEAD UAPI inventory)

- **2.0 set (ported, exposed):** `jc jmin jmax s1 s2 s3 s4 h1 h2 h3 h4 i1 i2 i3 i4 i5`
- **3.x-only set (stubbed off in the fork):** `header_protection_key`
  `content_padding_addition` `random_trailers` `disable_cookies` `rekey_after_time`
  `rekey_timeout` `reject_after_time` `keepalive_timeout` `max_handshake_attempts`

### A.4 `git diff --no-index --stat` — wireguard-go → amneziawg-go (`device/`)

Base `ecfc5a8d` (polyamide's exact base) vs port source `1b86b2a`; 29 files, +1706/−241:

```text
 device/bind_test.go              |   2 +-
 device/constants.go              |   1 +
 device/cookie.go                 |   3 +-
 device/cookie_test.go            |   2 +-
 device/device.go                 |  68 +++-
 device/device_test.go            | 151 +++++--
 device/keypair.go                |   2 +-
 device/noise-protocol.go         | 138 ++-----
 device/noise-types.go            | 108 +++++
 device/noise_test.go             |   4 +-
 device/obf.go                    | 143 +++++++
 device/obf_bytes.go              |  47 +++
 device/obf_data.go               |  25 ++
 device/obf_datasize.go           |  38 ++
 device/obf_datastring.go         |  29 ++
 device/obf_rand.go               |  39 ++
 device/obf_randchars.go          |  48 +++
 device/obf_randdigits.go         |  48 +++
 device/obf_timestamp.go          |  31 ++
 device/peer.go                   |  15 +-
 device/queueconstants_android.go |   2 +-
 device/queueconstants_default.go |   2 +-
 device/receive.go                | 162 +++++-
 device/send.go                   | 239 +++++++++--
 device/sticky_default.go         |   4 +-
 device/sticky_linux.go           |   4 +-
 device/timers.go                 | 151 +++++--
 device/tun.go                    |   2 +-
 device/uapi.go                   | 439 ++++++++++++++++++++-
 29 files changed, 1706 insertions(+), 241 deletions(-)
```

### A.5 `git diff --no-index --stat` — polyamide ↔ amneziawg-go (`device/`)

Fork collision surface; 37 files, +1976/−1174 (`→ /dev/null` = nylon-only file absent upstream;
`+N`-only = AWG-only file):

```text
 device/bind_test.go              |   2 +-
 device/constants.go              |   1 +
 device/cookie.go                 |   3 +-
 device/cookie_test.go            |   2 +-
 device/device.go                 | 113 ++--
 device/device_test.go            | 151 +++++-
 device/endpoint_test.go          |   4 -
 device/keypair.go                |   2 +-
 device/logger.go                 |   4 +-
 device/mobilequirks.go           |   6 +-
 device/noise-protocol.go         | 142 ++----
 device/noise-types.go            | 108 +++++
 device/noise_test.go             |   4 +-
 device/obf.go                    | 143 ++++
 device/obf_bytes.go              |  47 ++
 device/obf_data.go               |  25 ++
 device/obf_datasize.go           |  38 ++
 device/obf_datastring.go         |  29 ++
 device/obf_rand.go               |  39 ++
 device/obf_randchars.go          |  48 ++
 device/obf_randdigits.go         |  48 ++
 device/obf_timestamp.go          |  31 ++
 device/peer.go                   | 179 ++------
 device/pools.go                  |  16 -
 device/queueconstants_android.go |   2 +-
 device/queueconstants_default.go |   2 +-
 device/receive.go                | 278 +++++++++---
 device/send.go                   | 450 +++++++++++------
 device/status.go                 |  47 --   (nylon-only)
 device/sticky_default.go         |   4 +-
 device/sticky_linux.go           |  34 +-
 device/timers.go                 | 156 +++++--
 device/traffic_control.go        | 217 -----  (nylon-only)
 device/traffic_manip.go          | 214 -----  (nylon-only)
 device/traffic_manip_test.go     |  41 --   (nylon-only)
 device/tun.go                    |  16 +-
 device/uapi.go                   | 504 ++++++++++++++++++---
 37 files changed, 1976 insertions(+), 1174 deletions(-)
```

### A.6 nylon-local modifications (polyamide vs its base `ecfc5a8d`)

24 files, +981/−318. New: `status.go` (47), `traffic_control.go` (217), `traffic_manip.go` (214),
`traffic_manip_test.go` (41). Edited (line counts): `send.go` 241, `peer.go` 168, `receive.go` 126,
`device.go` 55, `uapi.go` 67, `sticky_linux.go` 34, `tun.go` 16, `pools.go` 16, `timers.go` 13,
`mobilequirks.go` 6, `noise-protocol.go` 6, `logger.go` 4, tests. No new UAPI case-keys were added
by nylon (knob set identical to upstream).

---

## Appendix B — member-record size computation

Representative record: `Schema=1`, 15-char node id, `Epoch/Version`, 32 B WG pubkey, 32 B ed25519
pubkey, 2 addresses (IPv4+IPv6), **10 endpoints** (~21 B each), **4 prefixes** (3×IPv4 /24, 1×IPv6
/64), `Obf{Protocol:"quic", I1–I5}` with **5 × ~100 B CPS strings**, 64 B ed25519 signature.

- Canonical binary (length-prefixed, fixed field order): **979 B**
- Compact JSON encoding of the same content: **1,223 B**

Both ≤ the 2,048 B record limit (§3.1); the JSON form is the conservative ceiling used for the
memberlist `NodeMeta` budget discussion (digests only, §3.3) and push-pull payload sizing
(~1.2 KB × 10 nodes ≈ 12 KB per exchange every 5 s).
