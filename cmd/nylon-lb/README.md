# nylon-lb — LoadBalancer controller for the nylon mesh

## Purpose

nylon-lb allocates an IPv4 address from a configured pool for every Service of
`type=LoadBalancer` and writes it to `status.loadBalancer.ingress`. It also
announces the allocated /32 into the nylon mesh: it binds the address on the
loopback interface and writes a dynamic-prefix file (`lb-<namespace>-<name>.json`)
into the node's `prefixes.d` directory, which the nylon daemon watches and
propagates — the same stand-proven pattern as `nylon-vip.service` +
`prefixes.d/00-vip.json`. Cilium in kube-proxy-replacement mode consumes
`status.loadBalancer.ingress` for the data plane; nylon-lb owns no datapath.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--pool` | (required) | IPv4 CIDR to allocate LoadBalancer addresses from. |
| `--exclude` | — | Repeatable IPv4 address never to allocate, e.g. the stand probe IP. |
| `--prefixes-dir` | `/etc/nylon/prefixes.d` | Directory nylon-lb writes `lb-*.json` dynamic-prefix files to. |
| `--bind-interface` | `lo` | Interface to bind allocated addresses on; empty string disables binding. |
| `--kubeconfig` | — | Path to a kubeconfig; falls back to in-cluster config, `$KUBECONFIG`, then `~/.kube/config`. |
| `--node-name` | hostname | Node name used for `externalTrafficPolicy=Local` endpoint matching. |
| `--leader-elect` | `true` | Run the allocator under leader election (one active allocator at a time). |
| `--leader-namespace` | in-cluster SA namespace, else `kube-system` | Namespace holding the `nylon-lb-allocator` Lease. |
| `--resync` | `30s` | Informer resync period. |
| `--health-addr` | `:9633` | Listen address for the `/healthz`, `/readyz`, and `/metrics` endpoints (empty disables the server). |
| `--speaker` | `true` | Announce/withdraw per-node /32s (prefix files + address binding). Set `false` for an allocator-only replica. |
| `--allocator` | `true` | Allocate addresses and update Service status. Set `false` for an announce-only replica. |
| `--log-level` | env `NYLON_LOG_LEVEL`, else `info` | Log level: `debug`, `info`, `warn`, or `error`. |
| `-v` / `--verbose` | `false` | Shorthand for debug-level logging. |
| `--json` | `false` | Log to stderr as JSON instead of tinted text. |

## Announce semantics

- `externalTrafficPolicy: Cluster` (default): every node announces the Service's
  /32 — Babel anycast across the mesh, with automatic failover when a node goes
  down. Traffic enters at the nearest announcing gateway.
- `externalTrafficPolicy: Local`: only nodes holding at least one ready local
  endpoint announce the /32; the announcement is withdrawn when the last ready
  endpoint leaves the node. Pass `--node-name` correctly (see the DaemonSet
  `NODE_NAME` note) or no node will ever match its endpoints.

## Logging

nylon-lb logs through the shared nylon logging pipeline. Every record carries
`component=nylon-lb` and `node=<name>` attributes; allocator records add
`module=allocator`, speaker records `module=speaker`.

- `--log-level debug|info|warn|error` sets the level (default `info`).
- `NYLON_LOG_LEVEL` is consulted when the flag is empty.
- `-v`/`--verbose` is a shorthand for debug level.
- `--json` switches stderr output from tinted text to JSON.

client-go internals (informer reflections, leader-election transitions) log
through klog; a klog bridge routes that chatter into the same slog pipeline,
so a single `--log-level` controls everything. `--version` prints the
version/commit stamped at link time (see `Makefile` `-ldflags`).

## Metrics and health endpoints

`--health-addr` (default `:9633`) serves three endpoints:

- `/healthz` — liveness. Always `200` once the process is serving; it does
  not check cluster connectivity.
- `/readyz` — readiness. `503` ("informer caches not synced") until the
  Service and EndpointSlice informer caches have synced, `200` afterwards.
  The DaemonSet readiness probe points here.
- `/metrics` — Prometheus text format:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `nylon_lb_build_info` | gauge | `version`, `commit` | Build stamp; always `1`. |
| `nylon_lb_leader` | gauge | `node` | `1` while this replica holds the allocator Lease. |
| `nylon_lb_allocated_ips` | gauge | — | Distinct pool addresses currently claimed by Service ingress or `spec.loadBalancerIP`. |
| `nylon_lb_services` | gauge | — | `type=LoadBalancer` Services currently in the cluster. |
| `nylon_lb_announces` | gauge | — | LoadBalancer /32s this node currently announces. |
| `nylon_lb_allocations_total` | counter | — | Fresh ingress assignments since start. |
| `nylon_lb_releases_total` | counter | — | Ingress clearances (Service deleted or no longer LoadBalancer) since start. |
| `nylon_lb_announce_writes_total` | counter | — | Successful announce-file writes since start. |
| `nylon_lb_errors_total` | counter | `kind` | Failed operations since start; `kind` is one of `bind` (address bind failure), `write` (announce-file write failure), `reconcile` (Service reconcile error), `list` (informer lister failure), `glob` (announce-file glob failure). |

Gauges refresh on each successful reconcile pass; `nylon_lb_leader` only
ever becomes `1` on the leader-elected allocator replica.

## Deployment

DaemonSet (recommended):

```bash
# local image (native platform, loaded into the daemon):
make image-nylon-lb REGISTRY=ghcr.io/encodeous/nylon IMAGE_TAG=latest
# multi-arch build & push to any registry:
make push-nylon-lb REGISTRY=ghcr.io/encodeous/nylon IMAGE_TAG=v0.4.0
# edit --pool/--exclude in deploy/daemonset.yaml (one pool per cluster)
kubectl apply -f cmd/nylon-lb/deploy/rbac.yaml
kubectl apply -f cmd/nylon-lb/deploy/daemonset.yaml
```

Bare binary / systemd (run as root on each node — the loopback binding needs
`NET_ADMIN`; pass `--bind-interface=` and a non-root user if you only need the
allocator):

```ini
[Service]
ExecStart=/usr/local/bin/nylon-lb \
  --pool=10.110.0.0/24 \
  --exclude=10.110.0.1 \
  --prefixes-dir=/etc/nylon/prefixes.d \
  --bind-interface=lo \
  --node-name=%H \
  --kubeconfig=/etc/nylon/kubeconfig
Restart=always
```

## Requirements and caveats

- The pool must be inside every receiver's `dynamic_prefix_ranges`; peers reject
  announced prefixes outside it (receiver-side fail-closed). Cross-zone traffic
  follows the gateway's static aggregate first, then the intra-zone /32.
- nylon-lb must be the ONLY LoadBalancer controller in the cluster: it owns ALL
  `type=LoadBalancer` Services (no `loadBalancerClass` filtering). Running it
  next to another controller (servicelb, MetalLB, kube-vip) corrupts allocations.
- Allocation state is derived from live Services, never stored. An IP can leak
  (stay unused) only if a Service is deleted while every replica is down; the
  next allocation still cannot collide, because announce files are garbage
  collected together with the Service. Same caveat as kube-vip.
- IPv4 only; one pool per cluster.

## Build

```bash
go build ./cmd/nylon-lb
# cross-compile for stand hosts:
GOOS=linux GOARCH=amd64 go build ./cmd/nylon-lb
```
