# Demo

A 3-node nylon stand (alice, bob, charlie) in Docker, plus a recorder that turns it into
the failover assets the docs use: [docs/assets/demo.gif](../docs/assets/demo.gif) (README hero)
and [docs/assets/demo.cast](../docs/assets/demo.cast) (the player on the docs landing page).

The recorded story: the direct alice–charlie link is cut with `iptables`, traffic reroutes through
bob, the link comes back and the route returns to direct. Every one of those transitions is asserted
against the daemon's own forwarding table before the recorder claims it happened — a run that does
not observe the reroute fails instead of producing a GIF.

## Requirements

- Docker with Compose v2
- `python3` (marker injection + cast checks in `record.sh`)

Nothing else runs on the host: `jq`, `mtr`, `fping`, `asciinema` and the daemon itself all live
in the image the scripts build.

## Entry points

### `./up.sh` — interactive stand, left running

Generates configs if needed, starts the three containers, warms the mesh up (see below) and blocks
until alice reaches charlie through the mesh route table with the backup path known (120s budget,
then it prints the daemon logs and exits non-zero). It prints the next hop toward charlie for each
node and a few commands to poke at the stand. The stand keeps running afterwards.

### `./record.sh` — record and install the demo assets

```bash
./record.sh              # record, then install into docs/assets/
./record.sh --no-install # record only, leave the files in demo/output/
```

It brings the stand up, records a 140x35 `tmux` layout with asciinema, converts the cast to a GIF
with [agg](https://github.com/asciinema/agg), and installs both files into `docs/assets/`.

Before anything is recorded, the stand is warmed up: `record.sh` (and `up.sh`) puts the mesh through
one cut/restore cycle of its own. The routing algorithm only accepts the backup route — bob
advertising `10.99.0.3/32` — while the mesh reconverges, so a cold stand can sit without it, and a cut
then waits for the algorithm's own timers and takes ~45s instead of the usual few seconds. The story
pane then asserts the warmed state before it cuts anything: it waits for the direct route *and* the
backup route to be known.

The recording is accepted only if:

- the panes observed all three transitions (`PASS converged`, `PASS reroute`, `PASS restored` in
  `demo/output/assertions.log`, and no `FAIL` line),
- the cast is an asciicast v2 with the 140x35 geometry the docs player renders it in.

Anything else fails the run — a green `record.sh` is a recording that proved the reroute.

A runtime watchdog (180s) kills the recording session if the story stalls, so the recorder can
never hang.

### `./generate-configs.sh` — configs only

Writes `demo/.env` (a free Docker subnet picked from the pool `e2e/network_allocator.go` uses),
three WireGuard keypairs and the `central.yaml` / `<node>.yaml` pair per node, then validates each
node config with `nylon verify`. Idempotent — existing configs are reused; pass `--force` to
regenerate.

## Layout

```text
demo/
├── lib.sh                # shared helpers: paths, docker compose, subnet picking, next-hop query
├── generate-configs.sh   # .env + keys + configs, validated
├── up.sh                 # interactive stand
├── record.sh             # recorder + asset installer
├── docker-compose.yml    # three nodes, subnet and addresses from demo/.env
└── panes/                # the four tmux panes the recording shows, run inside alice
    ├── mtr.sh            # traceroute across the mesh
    ├── ping.sh           # continuous ping
    ├── routes.sh         # alice's forwarding table, redrawn every second
    └── actions.sh        # the story: cut, assert the reroute, restore, assert again
```

## Teardown

```bash
docker compose --env-file demo/.env -f demo/docker-compose.yml -p nylon-demo down --remove-orphans
```

`demo/.env`, `demo/configs/` and `demo/output/` are generated and gitignored.

Takes about a minute (recording + GIF encoding); the first run additionally builds the image.

## Notes

- The subnet is picked at runtime, so the stand does not collide with other Docker networks and
  nothing in the scripts or configs is welded to `172.31.0.0/24`.
- Only the mesh addresses (`10.99.0.1`–`10.99.0.3`) are fixed: they are part of the story the
  recording tells.
- **Alice's link to bob is shaped to 20ms** (`pre_up` in the generated `alice.yaml`, `BOB_LINK_DELAY_MS`
  in `lib.sh`). On a docker bridge every link measures the same sub-millisecond latency with
  comparable jitter, so the mesh re-selects routes on noise and a "reroute" proves nothing. With the
  shaping, the direct alice–charlie path wins by ~20x and the path through bob is the only sensible
  fallback. `up.sh` and `record.sh` fail loudly if the shaping is missing.
