---
title: "Nylon Fork Design — polyamide-awg, Gossip Membership, Dynamic Prefixes, nylon-lb"
description: Design record for the nylon hard fork's features — polyamide-awg obfuscation, dynamic prefixes, k8s integration, and nylon-lb — with per-section implementation status.
---

Status: **design document** — the fork features have shipped: polyamide-awg obfuscation (§2), dynamic prefixes / `prefixes.d` (§4), nylon-lb (§6 — the shipped implementation differs; see its Implementation status note), and nylon-genesis; gossip membership (§3) and zones (§7) remain design-only.
Date: 2026-08-22, round 2 2026-08-24 · Base: `c3f872d98aad9d474927c108c74a94de571f4bfc` (`v0.4.5-4-gc3f872d`) · Branch: `fork/design`

Continuation of the earlier "Nylon + AmneziaWG self-healing underlay for k3s" design note (2026-08-16).
Changes vs that note: dynamic prefix contract is `prefixes.d/*.json` (not `*.conf`); nylon-lb is fully
designed (§6); the AWG wire-format target is locked to **2.0**; the polyamide↔amneziawg-go diff-audit
has been executed and its outcome is recorded here (§2, Appendix A).

Round 2 (this revision): org-wide zoned mesh (§7), control-plane VIP (§6.8), nylon-lb single-mode
failover hardening (§6.7); PoC plan renumbered to §8 (adds stage 5), risk register to §9 (R6–R7).

---

## 1. Overview & goals

Replace the Cilium Geneve overlay for k3s with a self-healing WireGuard-based mesh underlay built on
nylon. The mesh provides: any-to-any routed connectivity with Babel-style distance-vector convergence
(RTT-aware metrics), per-node prefix announcement with health-gated withdrawal, and — as a fork —
AmneziaWG 2.0 traffic obfuscation to make the underlay's handshakes indistinguishable from a chosen
ordinary protocol.

The mesh is org-wide and zoned: zones share one gossip domain with strict pod-CIDR isolation
between them and shared service slices for cross-zone reachability (§7), and the k3s control plane
floats on a mesh-announced anycast VIP (§6.8).

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

### 2.1 Target: AWG 2.0 wire format only

- **In:** the full 2.0 parameter surface — `Jc/Jmin/Jmax` (junk packets), `S1–S4` (message paddings),
  `H1–H4` (message-header ranges), `I1–I5` (custom signature packets, CPS tag grammar
  `<b 0x…>/<r n>/<rd n>/<rc n>/<t>`).
- **Out:** everything AWG 3.x adds on top — header protection (`HeaderProtectionKey`/HPK),
  `ContentPaddingAddition`, `random_trailers`, `disable_cookies`, and the timings overrides.
  These knobs are rejected at nylon's UAPI boundary (§2.3) and never written by nylon's config
  synthesis.
- **AWG 3.0 — deferred, not rejected:** the 3.x knobs stay stubbed (§2.3) until amnezigo gains
  3.x parameter support (separate task, user-driven). Upgrade when it happens: un-stub the
  rejected UAPI knobs, then swap the genesis profile via the §2.4 version-skew mechanism —
  a maintenance action (compat profile → 3.0 profile) converging within one rekey window. No 3.0
  design is committed in this document.
- amnezigo (`github.com/Arsolitt/amnezigo`, GPL-3.0) remains the **parameter oracle**, unchanged: nylon asks
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
Recorded fallback if PoC stage 2 (§8) finds HEAD's 2.0 encoding incompatible with reference AWG 2.0
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
  AWG 2.0 peer} — exercised in PoC stages 1–2 (§8). Reference peer lineage: the `1b86b2a` tree
  (published as `amneziaawg-go/v3 v3.1.20260814`, MIT); release `v0.2.19` predates the
  ranged-type/padding wire format and does **not** interoperate (lab leg 3, §8 outcome).
  Compatibility is regression-guarded automatically: (a) golden classification vectors in
  `polyamide/device/obf_golden_test.go` (46 upstream-derived expectations, always-on), and (b) an
  in-process harness `interop/awgref/` (`go test -tags awg_ref_interop ./interop/awgref/`) that
  handshakes and exchanges data against stock amneziaawg-go pinned in go.mod — retarget the module
  version + knob lines for the AWG 3.0 round.

### 2.5 Mandatory small patches

- **TUN MTU knob** — `LocalCfg.MTU *uint16` (new field; nil = today's behavior,
  `device.DefaultMTU = 1420`, `polyamide/device/tun.go:14`, fallback at `device/device.go:296–301`).
  The knob feeds amnezigo at genesis/generation time, which clamps I-packet sizes to
  `maxISize = MTU − reserve(49) − handshakeSize(149) − S1` (`amnezigo/cps.go:14–15,50–52`) and
  enforces `Jmax < MTU`. Rationale: I1–I5 and junk packets exceeding the underlay MTU fragment —
  fragmentation is DPI-visible and explicitly warned about in the amneziawg-go README.
- **TUN datapath drain** — multi-queue, non-blocking staging, batched writes, txqueuelen. Root
  cause: one TUN queue with one reader that blocks on the per-peer handoffs
  (`peer.queue.outbound.c`, `device.queue.encryption.c`), so a single stalled peer parks the reader
  and the per-queue kernel ring (`tx_queue_len`, default 500) overflows — 222k `tx_dropped` in
  ~1 day under a 65 MB/s replication burst, invisible to the daemon. Fix: `tun_queues` (kernel
  `ndo_select_queue` flow steering → per-flow affinity, so no reordering), a non-blocking reader
  with counted bounded per-peer queues, TUN writes batched off the receive goroutines,
  `tun_txqueuelen` (ring depth, ~1.4 KB/packet per queue at MTU 1420; programmed before the extra
  queues attach, because the ring is sized at attach time), and opt-in `tun_backpressure`
  (kernel >= 6.15) trading kernel-ring drops for qdisc queueing. Measured (Apple M5,
  `-benchtime 3s -count 1`, worktree-at-HEAD vs working tree): reader queues=1 571.5 → 562.6 ns/op
  (1.75M → 1.78M pkt/s), queues=4 577.5 → 393.5 ns/op (1.73M → 2.54M pkt/s, 1.45×); 2-node e2e at
  4 CPUs `tx_dropped` 279 → 0, iperf3 3.57 → 5.81 Gbit/s, retransmits 22555 → 15419; at 1 CPU
  `tx_dropped` 0 → 0, 2.99 → 3.31 Gbit/s, 6218 → 3864 retransmits (post-fix runs vary by up to
  ~20% run to run on the test host); control-plane `dispatch took a long time!` (a 5.28 ms
  `initWireGuard` closure) gone; `TCBatch` + callees 1.99% of a 92.79 s profile (10% gate, so no
  further optimization).
  Costs: ping-pong latency 37.0 → 40.7 µs, and under an unbounded-sender flood the counted
  userspace evictions replace kernel-ring drops (`BenchmarkThroughput` loss 0.21% → 80.6% — the
  loss was always there, now it is bounded and visible).
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
GPL-3.0; nylon is Apache-2.0. The fork keeps each file under its source license. The GPL-3.0 oracle
is linked only into the genesis tooling, so **distributed `nylon-genesis` binaries are GPL-3.0
combined works** (Apache-2.0 is one-way compatible with GPLv3): they may be published, provided the
GPL-3.0 text and the corresponding source travel with them — see `LICENSE.GPL-3.0` and the
provenance section of the genesis guide. The nylon daemon and `nylon-lb` link only MIT device code
plus Apache-2.0 nylon code and carry no copyleft obligations.

---

## 3. Gossip membership (design; not implemented this round)

> **Implementation status:** Design-only — gossip membership is not implemented and no gossip code exists in the repository. Membership and config distribution still flow through the central config today.

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

**`.nybundle` distribution is CANCELLED** (an earlier design decision). The distribution
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
- Files: `*.json`, **one writer per file** — the k8s pod-CIDR sidecar (`10-podcidr.json`) is design
  intent (§5); nylon-lb writes one announce file per Service (`lb-<namespace>-<service>.json`,
  cmd/nylon-lb/prefixfile.go:28-34). Writers must write tmp + rename (atomic).
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

fsnotify (`github.com/fsnotify/fsnotify` v1.9.0, go.mod:11) — shipped: `watchDynamicPrefixes`
(core/dynamic_prefixes.go:205-263, 250 ms debounce :21, initial scan at startup core/nylon.go:131)
on `dynamic_prefixes_dir`:

- Initial scan at startup (before first `ApplyCentralConfig`).
- Any event → 250 ms debounce → **full rescan** → rebuild set → `ApplyCentralConfig`.
- Delete or emptied file = withdraw: `reconcileAdvertisedPrefixes` stops the monitors and retracts
  the prefixes via the existing Babel machinery (`RouterState.Advertised` maintenance).

### 4.6 Receiver-side validation — `dynamic_prefix_ranges` (stage-3 addition)

Discovered during the stage-3 implementation: the routing protocol validates every incoming route
update against the *receiver's* central config (`checkPrefix`, exact prefix match in
`core/router.go`). A dynamic prefix exists only in the announcing node's injected view, so peers
would silently drop every dynamic announcement — cross-node podCIDR/anycast routing (§8 item 3)
is impossible without a receiver-side allowance.

- New `CentralCfg` field `dynamic_prefix_ranges []netip.Prefix`: a receiver accepts an announced
  prefix if it is **equal to a prefix in its central config** (existing rule, unchanged) or **a
  subnet of one of these ranges**. Applies to route updates, ack-retracts and seqno-requests.
- Empty list (default) = current fail-closed semantics: only centrally-known prefixes are routable.
  This is also the zone-isolation enforcement point (§7.3): zone synthesis gives each node only its
  zone aggregate + the shared slice, so foreign podCIDRs stay unroutable while shared VIPs
  (§7.4 slices) propagate.
- Supernets of a declared range are rejected; ranges must be masked and valid (validator).
- Deployment: a cluster declares `10.42.0.0/16` (pod CIDR aggregate — the same design-time
  constant as Cilium's `ipv4-native-routing-cidr`) plus the mesh VIP slice `10.60.0.0/24`.

---

## 5. k8s integration

> **Implementation status:** This section is design intent: no pod-CIDR sidecar has shipped (validated manually during PoC stage 3 (§8); any `prefixes.d` writer following §4 fills this role), and the checked-in `example/nylon.service` is still the minimal upstream unit — the `Restart=always`/`StartLimitIntervalSec=0`, `Type=notify`+`WatchdogSec`, and `Before=kubelet` hardening below is not shipped. The prerequisites it builds on have shipped: the `prefixes.d` contract (§4), the TUN MTU knob (§2.5), and `dynamic_prefix_ranges` receiver validation (§4.6).

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

> **Implementation status:** nylon-lb has shipped (`cmd/nylon-lb`) with a simpler architecture than this section: a single per-node DaemonSet runs one binary containing a **leader-elected allocator** and a **per-node speaker** (`--allocator`/`--speaker`, both default true; the allocator is leader-elected via a Lease, `--leader-elect`, default true). Allocation is stateless — derived from live Services against the named `--pool name=cidr` set (each Service selects its pool via the `nylon.io/lb-pool` annotation; lowest free address within the selected pool, `--exclude` protects reserved IPs), not persisted in a ConfigMap (§6.3) — and there is no finalizer and no `spec.externalIPs` write (§6.2): the allocator sets `status.loadBalancer.ingress` and honors `spec.loadBalancerIP` when it is free and inside the selected pool. The `anycast`/`single` modes (§6.4) have no flags; they are expressed per Service via `spec.externalTrafficPolicy` (`Cluster` — every node announces the /32; `Local` — only nodes with a ready endpoint). Announce files are one `lb-<namespace>-<service>.json` per Service in `--prefixes-dir` (default `/etc/nylon/prefixes.d`), and the speaker binds each announced /32 on `--bind-interface` (default `lo`) via netlink. §6.7's anti-split-brain TTL guard and §6.8 (control-plane VIP) are design-only: there is no `single`-mode announcer, no persisted lease/ConfigMap state to go stale, and no readyz-gated apiserver VIP in the shipped binary.

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

### 6.7 Single-mode liveness guard (anti-split-brain)

Scenario: a `single`-mode announcer (§6.4) is alive but partitioned from the k8s API while the
controller reassigns the VIP — two binders. Guard: the agent withdraws `20-nylon-lb.json` and
releases the VIP on `lo` when its observed `nylon-lb-state` ConfigMap is stale — `resourceVersion`
unchanged and the lease timestamp older than a **30 s** TTL (the controller refreshes the lease
every **10 s**) — or when the k8s API is unreachable for the same TTL. The split-brain window is
bounded by the TTL. `anycast` mode (the default, §6.4) is immune by construction: multiple owners
are the design, not a failure. TTL/refresh values are defaults, tunable at implementation time.

### 6.8 Control-plane VIP (anycast) — apiserver endpoint

Design decision: the floating apiserver VIP uses **anycast** mode (§6.4).

- **Address** — allocated from the zone's shared slice (§7.4) or a dedicated infra slice.
- **Announcement** — every k3s **server** node's nylon-lb agent announces the /32 and binds it on
  `lo`, gated on a local probe of `https://localhost:6443/readyz`: period **2 s**, 3 consecutive
  failures ⇒ withdraw + unbind, 1 success ⇒ re-announce (defaults, tunable).
- **k3s wiring** — agents get `server: https://<VIP>:6443`; servers MUST list the VIP in `tls-san`
  (cert SAN — client cert validation breaks without it).
- **Failover semantics** — node death ⇒ announcement retracts (Babel neighbour-death machinery,
  same auto-withdraw as §6.4) ⇒ clients land on the nearest healthy server. Long-lived watch
  connections break on reroute; client-go re-lists/re-watches transparently — the same failure
  class as the §6.4 anycast caveat.
- **etcd is NOT placed behind the VIP (design rule)** — etcd quorum members require stable
  identity; they keep their per-node mesh /32 addresses, announced by their own node always. The
  VIP covers only the stateless apiserver frontends.

---

## 7. Zones — org-wide multi-cluster mesh

> **Implementation status:** Design-only — zones are not implemented and no zone or gossip code exists in the repository.

### 7.1 Model

One gossip domain for the whole org (assumption: ≤ 50 nodes; the §3.3 memberlist push-pull cadence
stays fine at that size — ~1.2 KB × 50 ≈ 60 KB per exchange every 5 s, Appendix B sizing). A zone
is a **policy scope in the membership/synthesis layer, not a protocol construct** — Babel, the WG
data plane, and the wire format are zone-blind (§7.3). `MemberRecord` (§3.1) gains two fields:

```go
Zone    string // zone identifier (e.g. "msk", "spb")
Gateway bool   // cross-zone gateway role tag
```

≈ +16 B per record; against the measured budget of Appendix B (979 B canonical / 1,223 B compact
JSON) the 2 KB hard limit (§3.1) holds with wide margin.

### 7.2 Topology (synthesized `CentralCfg.Graph`)

Zone wiring rides the existing graph mechanism — `CentralCfg.Graph` (`state/config.go:44`) with
group syntax (`ParseGraph` doc, `state/config.go:153–168`). Synthesis (the per-node `CentralCfg`
synthesis of §3.5) emits for every node:

```text
zonea = <all zone-A member ids>
zoneb = <all zone-B member ids>
gwa   = <zone-A ids with Gateway=true>
gwb   = <zone-B ids with Gateway=true>
zonea, zonea   # intra-zone full mesh
zoneb, zoneb
gwa, gwb       # gateway pairs: full mesh across zones
```

Verified parser semantics: `g, g` interconnects every member pair and `g1, g2` interconnects the
two member sets pairwise (expansion pass, `state/config.go:251–324`); symbols are lowercased
(`:181`), and group names must not collide with node ids (`:189–191`). `GetPeers`
(`state/config.go:336–362`) **panics** on an invalid graph (`:344–347`) — a synthesis bug fails
loud at the offending node, never silently. WG adjacency follows the graph mechanically:
`syncWireGuardEndpoints` (`core/nylon_wireguard.go:199–235`) iterates `GetPeers(...)` (`:206`) to
program peer endpoints. Non-gateway nodes therefore never hold cross-zone tunnels.

### 7.3 Isolation mechanics (enforcement point)

Synthesis rule for a node N in zone Z — N's `CentralCfg` contains:

- (a) all zone-Z nodes with their **full** prefix lists;
- (b) foreign nodes with `Gateway=true`, carrying **only** the foreign zone's shared aggregate
  (§7.4) in `Prefixes`;
- foreign non-gateway node ids are omitted entirely.

Enforcement is upstream's existing validation — fail-closed on both axes:

- `routerHandleRouteUpdate` (`core/router.go:346–370`) accepts an update only if `checkPrefix`
  (`:327–335`) finds the prefix in the node's own `GetPrefixes()` — the union of all `Prefixes`
  entries in its `CentralCfg` view (`state/config.go:84–108`) — **and** `checkNode` (`:337–343`)
  knows the origin router id (`TryGetNode`, `state/config.go:435`). The same gates run in
  `routerHandleAckRetract` (`:372–385`) and `routerHandleSeqnoRequest` (`:387–404`).
- A route never installed in `RouterState.Routes` never becomes an OS route:
  `ComputeSysRouteTable` (`core/router.go:252–275`) derives the system table from
  `RouterState.Routes` only — no FIB entry ⇒ no system route ⇒ sender-side drop, and packets
  injected into the TUN drop too (no forwarding entry).

Consequence: a zone-Z node never installs routes for foreign podCIDRs — no reachability **and** no
transit (nothing to forward through). Within-zone transit and multi-hop behave exactly as today.

**Dynamic-prefix bridge (design intent).** Runtime announcements via `prefixes.d` (§4) — VIP /32s
from nylon-lb agents — enter a receiver's `GetPrefixes()` union only if the announcer's member
record carries them: the record publisher sources `MemberRecord.Prefixes` from the merged
announced set (static + dynamic) and re-publishes on change (Version bump; §3.1 merge rules), so
peers re-synthesize and `checkPrefix` admits the new prefix. Without this bridge `checkPrefix`
would drop every dynamic announcement at the first hop — PoC stages 3b and 5(b) (§8) exercise it
end-to-end.

### 7.4 Shared subnets

Each zone carves a shared slice from the mesh supernet at design time. Example layout (real
allocation happens at fleet design time):

| Block | Example | Purpose |
|---|---|---|
| Org supernet | `10.0.0.0/8` | whole mesh |
| Zone block | `10.0.0.0/14` … `10.48.0.0/14` | node /32s + per-node podCIDR /24s |
| Zone shared slice | `10.0.16.0/20` (carved from the zone block) | exposed services + nylon-lb VIP pool |

Zone gateways announce the zone's shared aggregate (static health, anycast across the zone's
gateways) via their synthesized own-entry `Prefixes`; foreign views home the aggregate on those
gateways (§7.3b). nylon-lb controllers allocate Service VIPs from the owning zone's shared slice —
§6.3 extends to: pool = one ConfigMap per cluster, carved from its zone's shared slice. Routing
hierarchy: foreign node → foreign gateway (aggregate) → inside the owning zone the VIP /32 (more
specific, announced by lb agents via the §7.3 bridge) wins longest-prefix match.

### 7.5 Accepted trade-off — cross-zone transit of shared traffic

Zone-Z nodes hold routes to foreign shared aggregates, so when all intra-zone paths to a service's
announcers are degraded, Babel may route shared traffic through a foreign gateway. Bounded to
shared aggregates only (podCIDR isolation is unaffected, §7.3), hop-by-hop encrypted (§1), and
self-healing by design. Documented as accepted; PoC stage 5 asserts the bound (§8).

### 7.6 Gateway deployment model

Gateway = a role tag on the member record, not a node kind. Initial deployment: one repurposed
k3s **worker** per zone; the worker keeps scheduling pods.
Production requirement: **≥ 2 gateways per zone** (R7) before carrying production cross-zone
traffic. Migration to dedicated VMs later = flip `Gateway` on the records and let synthesis +
Babel reconverge; no cluster re-deploy. Draining a gateway follows the R4 runbook (§5) plus a
cross-zone convergence check (§8 stage 5).

### 7.7 Policy boundary

CiliumNetworkPolicy filters at the destination cluster's edge (pod ingress). Non-k8s hosts on the
mesh are NOT covered by CNP — L3 scoping (zone isolation + shared-only reachability, §7.3) is
their only control. Stated explicitly: L4/L7 policy exists only where Cilium runs.

---

## 8. PoC plan

1. **Vanilla nylon** — 3 VMs, netem break A↔B: convergence time, throughput, long-lived TCP
   survival across link failure/recovery.
2. **polyamide-awg** — pcap: handshake indistinguishable from the mimicked protocol (target from
   `Obf.Protocol` template); interop matrix of §2.4 (compat profile vs vanilla; obf profile
   node-to-node; reference AWG 2.0 peer); re-benchmark throughput (R1 perf gate).
   **Stage-2 outcome (2026-08-24, fork/awg @ 63c4349, test stand legs 1–3 + iperf3):** leg1
   vanilla(c3f872d)↔compat — plain WG types 1/2/4 on the wire (pcap: 2×init, 1×resp, 68×transport
   at offset 0), 5/5 ping; leg2 obf↔obf (quic profile) — 0 vanilla type words in 45 payloads,
   5/5 ping; leg3 obf↔amneziawg-go handshake+data OK against `1b86b2a` (keepalives both ways,
   4/4 injected ICMP arrive on nylon tun) — but **not** against `v0.2.19`: its wire format predates
   the ranged-type/padding machinery (verdict row §2.3: reference is the 1b86b2a lineage, not
   v0.2.19); note a bare AWG peer can never complete nylon-protocol pings (nylon wraps all data in
   poly bundles) — raw-ICMP injection is the correct probe. iperf3 A/B same nodes/link: vanilla
   433/429 Mbit/s fwd, 224/222 rev; obf 418/415 fwd, 226/223 rev (≤4% cost).
3. **k3s + Cilium native + `prefixes.d` JSON** — break the direct link between two nodes: verify
   pod-to-pod, apiserver, etcd, and in-cluster DB reachability across the healed path.
   **3b.** two-node same-/32 announce → anycast check for §6 (§6.5 prerequisites).
   **Stage-3 outcome (2026-08-26, fork/awg @ fcc32ed, test stand, items 3 + 3b):** feature legs —
   daemon `prefixes.d` contract deployed on all 4 nodes (watcher active, mesh 4/4); Cilium 1.19.5
   switched tunnel/geneve → `routingMode: native` + `ipv4NativeRoutingCIDR: 10.42.0.0/16` +
   `MTU: 1380` over `nylon0`. Baseline before announces: cross-node pod ping 100% loss (the
   announce step is load-bearing). B3: per-node podCIDR `10-podcidr.json` (metric 0) — peers select
   all four `10.42.X.0/24` with correct source routers; kernel routes via nylon0 (contiguous /24s
   coalesce, e.g. `10.42.0.0/23`); cross-node pod ping 3/3 @ 5.5 ms. B4 (partition node-1↔node-2,
   nft pairwise public-IP drop): mesh /32 heal **3 s**; dynamic podCIDR route heal ≈ **20–25 s**
   (seqno-request cycle — the route lags the mesh ping heal); healed-path matrix all green — pod-pod
   ping both directions 3/3 (4.7/6.7 ms), apiserver TCP reachable from pod and from the partitioned
   agent host (401 in 19–40 ms, `k3s-agent` 0 reconnect errors), etcd `[+]ok`, ClusterIP DNS
   `kubernetes.default` → 10.43.0.1; underlay relay proof on a transit hop (capture: identical
   payload lengths in and out of the forwarding node). Unpartition: direct next-hop restored in **102 s**
   (Babel route aging), ping 3/3 @ 2.5 ms. B5 (anycast VIP `10.60.0.100/32` from node-2+node-3):
   selection node-3 (metric 2025 < 3646); withdraw on the serving node → next-hop flip to
   node-2 in **1 s**, continuous ping 396/400 (**loss window ≤ 0.8 s**, zero unreachables), nylon
   forward table — the WG AllowedIPs source — reprogrammed `10.60.0.100/32 → node-2`; cleanup
   withdraw verified (0 routes). Deviations from the plan: (1) §4.6 `dynamic_prefix_ranges` had to
   be added — receivers otherwise drop dynamic announcements in `checkPrefix` (cross-node routing
   was impossible without it); (2) Cilium chart value keys are case-sensitive camelCase —
   `ipv4-native-routing-cidr`/`mtu` are silently ignored (`ipv4NativeRoutingCIDR`/`MTU` required;
   the pre-stage `mtu: 1360` never applied, auto-MTU was in effect); (3) agent hosts have no
   kubeconfig — apiserver leg measured with host curl instead of `kubectl`.
4. **Soak** — random degradation schedule (link loss, latency injection, node restarts); also
   exercises Babel RTT de-preference.
5. **Two-zone lab (§7)** — reuse a 4-node test stand as 2 zones × 2 nodes; in each zone the
   "extra" node carries the gateway role; vanilla nylon (zones are synthesis policy, §7.3 —
   per-node graph/prefix views assembled by hand):
   - (a) zone-B node has NO route to the zone-A podCIDR (routes section of `nylon status` empty
     for it; ping fails);
   - (b) shared VIP — a loopback-bound /32 announced via a test `prefixes.d` file (§7.3 dynamic
     bridge) — reachable cross-zone through the zone gateways;
   - (c) A-internal partition (existing nft partition tooling): tcpdump on zone-B `nylon0` shows
     zero packets with zone-A podCIDR src/dst — transit-block proof (§7.3);
   - (d) apiserver VIP anycast on the test stand's k3s servers (§6.8): stop nylon on one server node
     ⇒ the kubelet on a worker reconnects to the remaining server within its retry window;
     `tls-san` configured per §6.8.

---

## 9. Risk register

| # | Risk | Mitigation |
|---|---|---|
| R1 | AWG port costs throughput (padding/junk/extra copies) | perf gate first-class in PoC stage 2; abort criterion if mesh throughput drops beyond agreed budget vs vanilla |
| R2 | Upstream fragility (wireguard-go/amneziawg-go drift breaks the port) | patchset-over-tags strategy (§1); pinned bases; e2e + netem + pcap CI in the fork; version-skew wire compat as release criterion (§2.4) |
| R3 | k8s IPAM changes `podCIDR` on node recreation | sidecar rewrites `10-podcidr.json`; withdraw/re-announce handled by existing health/Babel machinery; acceptance covered in PoC stage 3 |
| R4 | nylon is a per-node SPOF | systemd `Restart=always` + `StartLimitIntervalSec=0` + watchdog (§2.5b); drain/maintenance procedures (§5) |
| R5 | `prefixes.d` is an unauthenticated local control surface | directory root-owned `0755`, files root-owned `0644`; only root/hostPath writers; fail-closed parsing (§4.4) |
| R6 | zone synthesis bug leaks or strands routes | fail-closed by upstream `checkPrefix`/`checkNode` — unknown prefix or origin ⇒ drop (`core/router.go:327–343`, §7.3); e2e zone suite in CI (stage 5 assertions automated, §8) |
| R7 | single gateway per zone = cross-zone SPOF (worker-as-gateway, §7.6) | accepted temporarily; gate: ≥ 2 gateways per zone before production cross-zone traffic; monitor `nylon_selected_routes` / `nylon_route_metric` (`docs/guides/observability.mdx`) |

---

## Appendix A — audit evidence

### A.1 Revisions

| Repo | Commit | Notes |
|---|---|---|
| nylon (this fork) | `c3f872d98aad9d474927c108c74a94de571f4bfc` | `v0.4.5-4-gc3f872d`, branch `fork/design` |
| polyamide base (wireguard-go) | `ecfc5a8d54462e18e13c72173e2623d16d8e25a0` | vendored via merges `9a360bb`/`2a93dbf`; module `github.com/encodeous/nylon/polyamide` |
| amneziawg-go (port source) | `1b86b2ae0e493e7ea93f8c1a0f0cb6735b1551f1` | tag `v3.1.20260814`, 2026-08-13; module `github.com/amnezia-vpn/amneziawg-go/v3`; device/ MIT |
| amneziawg-go fallback | `1cc94272ca8e9e223a5fe76382f5880f09d3c12d` | tag `v0.2.19` — newest tag with zero HPK references in `device/` (last with `magic-header.go`) |
| amnezigo | `github.com/Arsolitt/amnezigo` @ `v0.3.0` (GPL-3.0) | oracle; `cps.go`, `presets.go`, `protocols.go`, `validation.go` |

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

Fork collision surface; 30 files, +795/−2446 against amneziawg-go
`1b86b2ae0e493e7ea93f8c1a0f0cb6735b1551f1` (tag `v3.1.20260814`, the §2.2 port source). The first
path is `polyamide/device`, so `−` = lines only polyamide has and `+` = lines only upstream has;
`→ /dev/null` = file present in polyamide but absent upstream — all nylon-only now that the tree
carries the port:

```text
 device/bind_test.go              |   2 +-
 device/datapath_perf_test.go     | 748 --------------------- (nylon-only)
 device/device.go                 | 119 +---
 device/device_test.go            | 151 ++++-
 device/endpoint_test.go          |   4 -
 device/keypair.go                |   2 +-
 device/logger.go                 |   4 +-
 device/mobilequirks.go           |   6 +-
 device/noise-protocol.go         |   6 +-
 device/noise_test.go             |   4 +-
 device/obf_golden_test.go        | 137 ----                  (nylon-only)
 device/obf_test.go               | 158 -----                 (nylon-only)
 device/peer.go                   | 206 ++----
 device/pools.go                  |  16 -
 device/queueconstants_android.go |   3 +-
 device/queueconstants_default.go |   3 +-
 device/queueconstants_ios.go     |   1 -
 device/queueconstants_windows.go |   1 -
 device/receive.go                | 158 +++--
 device/send.go                   | 337 ++++------
 device/status.go                 |  47 --                    (nylon-only)
 device/sticky_default.go         |   4 +-
 device/sticky_linux.go           |  34 +-
 device/timers.go                 | 150 ++++-
 device/traffic_control.go        | 210 ------                (nylon-only)
 device/traffic_manip.go          | 219 ------                (nylon-only)
 device/traffic_manip_test.go     |  41 --                    (nylon-only)
 device/tun.go                    |  16 +-
 device/tunwrite.go               | 175 -----                 (nylon-only)
 device/uapi.go                   | 279 ++++++--
```

The ported `obf*` family no longer shows up here (polyamide and amneziawg-go agree on it byte for
byte); the surviving rows are the nylon traffic-control/datapath modification set (`send.go`,
`uapi.go`, `peer.go`, `receive.go`, `device.go`) plus small per-file drift (module-path renames and
the like). Recompute: `git diff --no-index --stat polyamide/device <amneziawg-go v3.1.20260814>/device`.

### A.6 polyamide vs its base `ecfc5a8d` — full fork delta

43 files, +466/−3582 against wireguard-go `ecfc5a8d54462e18e13c72173e2623d16d8e25a0`. The first
path is `polyamide/device`, so `−` = lines only polyamide has (nylon-local/ported code) and `+` =
lines only upstream has; `→ /dev/null` = file present in polyamide but absent at the base rev — the
ported `obf*` family plus the nylon-only files:

```text
 device/bind_test.go              |   2 +-
 device/constants.go              |   1 -
 device/cookie.go                 |   3 +-
 device/cookie_test.go            |   2 +-
 device/datapath_perf_test.go     | 748 --------------------- (fork-only)
 device/device.go                 | 147 +---
 device/device_test.go            |   8 +-
 device/endpoint_test.go          |   4 -
 device/keypair.go                |   2 +-
 device/logger.go                 |   4 +-
 device/mobilequirks.go           |   6 +-
 device/noise-protocol.go         | 142 ++--
 device/noise-types.go            | 108 ---
 device/noise_test.go             |   4 +-
 device/obf.go                    | 143 ----                  (fork-only)
 device/obf_bytes.go              |  47 --                    (fork-only)
 device/obf_data.go               |  25 -                     (fork-only)
 device/obf_datasize.go           |  38 --                    (fork-only)
 device/obf_datastring.go         |  29 -                     (fork-only)
 device/obf_golden_test.go        | 137 ----                  (fork-only)
 device/obf_rand.go               |  39 --                    (fork-only)
 device/obf_randchars.go          |  48 --                    (fork-only)
 device/obf_randdigits.go         |  48 --                    (fork-only)
 device/obf_test.go               | 158 -----                 (fork-only)
 device/obf_timestamp.go          |  31 -                     (fork-only)
 device/peer.go                   | 205 ++----
 device/pools.go                  |  16 -
 device/queueconstants_android.go |   3 +-
 device/queueconstants_default.go |   3 +-
 device/queueconstants_ios.go     |   1 -
 device/queueconstants_windows.go |   1 -
 device/receive.go                | 254 +++----
 device/send.go                   | 536 ++++-----------
 device/status.go                 |  47 --                    (fork-only)
 device/sticky_default.go         |   4 +-
 device/sticky_linux.go           |  34 +-
 device/timers.go                 |  19 +-
 device/traffic_control.go        | 210 ------                (fork-only)
 device/traffic_manip.go          | 219 ------                (fork-only)
 device/traffic_manip_test.go     |  41 --                    (fork-only)
 device/tun.go                    |  16 +-
 device/tunwrite.go               | 175 -----                 (fork-only)
 device/uapi.go                   | 340 +---------
```

No new UAPI case-keys were added by nylon itself: the fork's knob set matches the A.4 port source
exactly (the 3.x-only keys are rejected in `handleDeviceLine`). Recompute:
`git diff --no-index --stat polyamide/device <wireguard-go-at-ecfc5a8d>/device`.

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
