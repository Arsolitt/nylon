#!/bin/bash
# Records the failover demo and installs the assets the docs consume.
#
#   ./record.sh               record and install into docs/assets/
#   ./record.sh --no-install  record only, leave the files in demo/output/
#
# The recording is accepted only if the pane scripts observed the reroute for
# real: demo/output/assertions.log must carry three PASS lines (converged,
# reroute, restored) and no FAIL line, and the cast must be an asciicast v2 with
# the 140x35 geometry the docs player renders it in.
#
# Requires: docker + docker compose v2, python3.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

INSTALL=1
case "${1:-}" in
"") ;;
--no-install) INSTALL=0 ;;
*) die "usage: $(basename "$0") [--no-install]" ;;
esac

require_cmd docker
require_cmd python3

cleanup() {
  info "stopping the stand"
  dc down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

ASSERTIONS="$OUTPUT_DIR/assertions.log"
CAST="$OUTPUT_DIR/demo.cast"
GIF="$OUTPUT_DIR/demo.gif"
MARKERS="$OUTPUT_DIR/markers.txt"

# ── 1. Stand ─────────────────────────────────────────────────────────
"$DEMO_DIR/generate-configs.sh"
load_env || die "$ENV_FILE was not written — cannot start the stand"

info "resetting $OUTPUT_DIR"
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

info "starting the mesh"
dc up -d --force-recreate
wait_for_shaping 30
warm_up 120

# ── 2. Pane scripts ──────────────────────────────────────────────────
ALICE="$(container alice)"
docker exec "$ALICE" mkdir -p /tmp/panes
docker cp "$PANES_DIR/." "$ALICE:/tmp/panes/"
docker exec "$ALICE" sh -c 'chmod +x /tmp/panes/*.sh'

# ── 3. Record ────────────────────────────────────────────────────────
# 140x35 is the geometry the docs player renders the cast in; the pane layout
# below (left column 85 wide, right column 55, routes pane 9 rows) fills it.
info "recording — the mesh is converged and warmed up, the story pane waits for both"
docker exec -i -e TERM=xterm-256color -e CHARLIE_IP="$CHARLIE_IP" "$ALICE" bash -s <<'RECORDER'
set -euo pipefail

asciinema rec /tmp/demo.cast --cols 140 --rows 35 --overwrite -c "
  tmux new-session -d -s demo -x 140 -y 35 /tmp/panes/mtr.sh
  tmux split-window -h -t demo:0.0 -l 55 /tmp/panes/actions.sh
  tmux split-window -v -t demo:0.1 -l 9 /tmp/panes/routes.sh
  tmux split-window -v -t demo:0.0 -l 20 /tmp/panes/ping.sh
  exec tmux attach -t demo
"
RECORDER

info "recording finished"

# ── 4. Pull the artifacts out of the container ───────────────────────
docker cp "$ALICE:/tmp/demo.cast" "$CAST" || die "no cast was written — did the pane scripts run?"
docker cp "$ALICE:/tmp/assertions.log" "$ASSERTIONS" || die "no assertions were written — did the pane scripts run?"
docker cp "$ALICE:/tmp/markers.txt" "$MARKERS" || die "no markers were written — did the pane scripts run?"

if docker exec "$ALICE" test -e /tmp/watchdog-fired; then
  cat "$ASSERTIONS" >&2
  die "the pane watchdog fired: the story never finished (see $ASSERTIONS)"
fi

# ── 5. Gates: the recording must prove the reroute happened ──────────
for name in converged reroute restored; do
  grep -q "^PASS $name " "$ASSERTIONS" || {
    cat "$ASSERTIONS" >&2
    die "the recording did not observe '$name' (see $ASSERTIONS)"
  }
done
if grep -q '^FAIL ' "$ASSERTIONS"; then
  cat "$ASSERTIONS" >&2
  die "the recording observed a failure (see $ASSERTIONS)"
fi
info "assertions: $(tr '\n' ' ' <"$ASSERTIONS")"

# ── 6. Markers ───────────────────────────────────────────────────────
info "injecting markers into the cast"
python3 - "$MARKERS" "$CAST" <<'PY'
import json
import sys

markers_path, cast_path = sys.argv[1], sys.argv[2]

markers = []
with open(markers_path) as f:
    for line in f:
        ts, _, label = line.strip().partition(" ")
        if label:
            markers.append((float(ts), label))

with open(cast_path) as f:
    header = json.loads(f.readline())
    events = [json.loads(line) for line in f if line.strip()]

events += [[ts, "m", label] for ts, label in markers]
events.sort(key=lambda event: event[0])

with open(cast_path, "w") as f:
    f.write(json.dumps(header) + "\n")
    for event in events:
        f.write(json.dumps(event) + "\n")

print(f"  injected {len(markers)} markers: {', '.join(label for _, label in markers)}")
PY

python3 - "$CAST" <<'PY'
import json
import sys

with open(sys.argv[1]) as f:
    header = json.loads(f.readline())
    events = [json.loads(line) for line in f if line.strip()]

if header.get("version") != 2:
    sys.exit(f"not an asciicast v2: {header}")
if (header.get("width"), header.get("height")) != (140, 35):
    sys.exit(f"unexpected cast geometry: {header}")
markers = [e[2] for e in events if len(e) == 3 and e[1] == "m"]
print(f"  cast: v2 {header['width']}x{header['height']}, {len(events)} events, markers: {markers}")
PY

# ── 7. GIF ───────────────────────────────────────────────────────────
info "converting to GIF"
docker run --rm -u "$(id -u):$(id -g)" \
  -v "$OUTPUT_DIR:/data" \
  ghcr.io/asciinema/agg@sha256:84e04c21013e4fb91cbdb3eade5977c1e66685c18f0d234da78d3350bb3404b2 \
  /data/demo.cast /data/demo.gif \
  --font-size 16 \
  --idle-time-limit 2.0 \
  --speed 1.0 \
  --theme asciinema

[ -f "$GIF" ] || die "agg produced no GIF (cast: $CAST)"

# ── 8. Install ───────────────────────────────────────────────────────
if [ "$INSTALL" = 1 ]; then
  info "installing into $REPO_ROOT/docs/assets"
  install -m 0644 "$CAST" "$REPO_ROOT/docs/assets/demo.cast"
  install -m 0644 "$GIF" "$REPO_ROOT/docs/assets/demo.gif"
  cmp "$CAST" "$REPO_ROOT/docs/assets/demo.cast" || die "demo.cast did not install cleanly"
  cmp "$GIF" "$REPO_ROOT/docs/assets/demo.gif" || die "demo.gif did not install cleanly"
  info "docs/assets/demo.cast and docs/assets/demo.gif are up to date"
else
  info "skipping the docs/assets install (--no-install)"
fi

ls -lh "$CAST" "$GIF"
