#!/bin/bash
# Right-hand pane of the recorded session: drives the failover story.
#
# The point of this script is that it never claims a success it did not observe.
# Every step polls alice's own forwarding table and only then prints the ✓ line,
# writes the cast marker and appends "PASS <name> <elapsed>s" to
# /tmp/assertions.log. A step that does not happen within its bound appends
# "FAIL <name> timeout", kills the session and exits non-zero, which fails the
# recording on the host side (see demo/record.sh).
#
# Runs inside nylon-demo-alice. $CHARLIE_IP (charlie's docker address) is injected
# by the recorder, so nothing here is welded to a particular subnet.

TARGET_PREFIX="${TARGET_PREFIX:-10.99.0.3/32}"
CHARLIE_IP="${CHARLIE_IP:?CHARLIE_IP must be set}"
WATCHDOG_SECONDS="${WATCHDOG_SECONDS:-180}"

ASSERTIONS=/tmp/assertions.log
MARKERS=/tmp/markers.txt

# ── Color helpers ────────────────────────────────────────────────────
R="\033[0m" # reset
BOLD="\033[1m"
RED="\033[31m"
GREEN="\033[32m"
CYAN="\033[36m"
DIM="\033[90m"
BRED="\033[1;31m"
BGREEN="\033[1;32m"
BYELLOW="\033[1;33m"
BCYAN="\033[1;36m"

START=$(date +%s)
elapsed() { echo "$(( $(date +%s) - START ))s"; }
marker() { echo "$(( $(date +%s) - START )) $1" >>"$MARKERS"; }
header() { echo -e "${BYELLOW}── $* ──${R}"; }
ok() { echo -e "${BGREEN}  [$(elapsed)] ✓ $*${R}"; }
info() { echo -e "${DIM}  $*${R}"; }

BACKUP_ID="bob" # where the reroute must go

# ── Route table ──────────────────────────────────────────────────────
nh() { # next hop toward $TARGET_PREFIX, straight from the daemon
  nylon status --json -i nylon0 2>/dev/null |
    jq -r --arg p "$TARGET_PREFIX" '.status.routes.forward[]? | select(.prefix==$p) | .nh' 2>/dev/null | head -1
}

# Has the backup path been advertised to us yet? Cutting the link before bob has
# advertised the target prefix would race the routing algorithm's own update
# timers (and turn a ~6s reroute into a ~45s one), so the story waits for it.
has_standby() {
  nylon status --json -i nylon0 2>/dev/null |
    jq -e --arg p "$TARGET_PREFIX" --arg n "$BACKUP_ID" '
      any(.status.neighbours[]?; .peerId == $n and any(.routes[]?; .pubRoute.source.prefix == $p))' \
      >/dev/null 2>&1
}

# ── Watchdog: the recording can never hang ───────────────────────────
WATCHDOG_PID=""
stop_watchdog() { [ -n "$WATCHDOG_PID" ] && kill "$WATCHDOG_PID" 2>/dev/null || true; }
trap 'stop_watchdog' EXIT TERM HUP INT

(
  sleep "$WATCHDOG_SECONDS"
  echo "FAIL watchdog timeout" >>"$ASSERTIONS"
  touch /tmp/watchdog-fired
  tmux kill-session -t demo 2>/dev/null
) &
WATCHDOG_PID=$!

# ── Topology diagrams ────────────────────────────────────────────────
topo_normal() {
  echo -e "  ${BOLD}alice${R} .1 ────── ${BOLD}charlie${R} .3"
  echo -e "  ${BOLD}${R}       |           |"
  echo -e "  ${BOLD}bob${R}   .2 ──────────┘"
}

topo_broken() {
  echo -e "  ${BOLD}alice${R} .1 ${RED}──✗───${R} ${BOLD}charlie${R} .3"
  echo -e "  ${BOLD}${R}       |           |"
  echo -e "  ${BOLD}bob${R}   .2 ──────────┘"
}

topo_rerouted() {
  echo -e "  ${BOLD}alice${R} .1 ${RED}──✗───${R} ${BOLD}charlie${R} .3"
  echo -e "  ${BOLD}${R}       ${GREEN}|${R}           ${GREEN}|${R}"
  echo -e "  ${GREEN}${BOLD}bob${R}   .2 ${GREEN}──────────┘${R}"
}

topo_restored() {
  echo -e "  ${BOLD}alice${R} .1 ${GREEN}──────${R} ${BOLD}charlie${R} .3"
  echo -e "  ${BOLD}${R}       |           |"
  echo -e "  ${BOLD}bob${R}   .2 ──────────┘"
}

# ── Assertions ───────────────────────────────────────────────────────
bail() { # <name> <detail> — no observed transition, the recording fails
  echo -e "${R}"
  echo -e "${BRED}  [$(elapsed)] ✗ $1 — $2 (backup path known: $(has_standby && echo yes || echo no))${R}"
  echo ""
  info "daemon logs: docker compose --env-file demo/.env -f demo/docker-compose.yml -p nylon-demo logs --tail=100"
  echo "FAIL $1 timeout" >>"$ASSERTIONS"
  tmux kill-session -t demo 2>/dev/null || true
  exit 1
}

wait_nh() { # <expected hop> <timeout_s> <name> [standby]
  local expected="$1" bound="$2" name="$3" want_standby="${4:-0}" got deadline
  deadline=$(( $(date +%s) + bound ))
  echo -ne "${DIM}  [$(elapsed)] waiting for $TARGET_PREFIX via ${expected}"
  [ "$want_standby" = 1 ] && echo -ne " (backup path known)"
  while :; do
    got="$(nh)"
    if [ "$got" = "$expected" ] && { [ "$want_standby" = 0 ] || has_standby; }; then
      break
    fi
    [ "$(date +%s)" -ge "$deadline" ] &&
      bail "$name" "next hop stayed '${got:-unknown}', expected '$expected'"
    echo -ne "."
    sleep 0.5
  done
  echo -e "${R}"
  echo "PASS $name $(( $(date +%s) - START ))s" >>"$ASSERTIONS"
  ok "$name"
}

# ── The story ────────────────────────────────────────────────────────
: >"$ASSERTIONS"
: >"$MARKERS"
rm -f /tmp/watchdog-fired

echo -e "${BCYAN}# nylon — self-healing WireGuard mesh${R}"
echo ""
header "topology"
echo ""
topo_normal
echo ""

# 1. Converged: alice reaches charlie directly, and already knows the path through
#    bob — only then is cutting the link a fair test of the failover.
wait_nh charlie 120 converged 1
marker "Mesh converged"
info "route: alice → charlie (direct, 1 hop)"
echo ""

sleep 12

# 2. Cut the direct link.
header "cutting direct link"
echo ""
topo_broken
echo ""
marker "Link cut"
echo "  $ iptables -A INPUT -s $CHARLIE_IP -j DROP"
iptables -A INPUT -s "$CHARLIE_IP" -j DROP || bail cut "iptables -A INPUT failed"
echo "  $ iptables -A OUTPUT -d $CHARLIE_IP -j DROP"
iptables -A OUTPUT -d "$CHARLIE_IP" -j DROP || bail cut "iptables -A OUTPUT failed"
echo ""

# 3. Reroute through bob — asserted, not assumed.
wait_nh bob 30 reroute
marker "Rerouted via bob"
info "route: alice → bob → charlie (2 hops)"
echo ""
topo_rerouted
echo ""

sleep 12

# 4. Restore the direct link.
header "restoring direct link"
echo ""
echo "  $ iptables -F"
iptables -F || bail restore "iptables -F failed"
echo ""

wait_nh charlie 60 restored
marker "Direct link restored"
info "route: alice → charlie (direct, 1 hop)"
echo ""
topo_restored
echo ""

echo -e "${BCYAN}  Zero config changes. Routes healed automatically.${R}"
echo -e "${BCYAN}  https://github.com/Arsolitt/nylon${R}"
echo ""

sleep 5

tmux kill-session -t demo 2>/dev/null || true
