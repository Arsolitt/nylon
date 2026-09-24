#!/bin/bash
# Bottom-right pane of the recorded session: alice's own forwarding table for the
# three mesh addresses, redrawn every second. This is the pane that shows the
# reroute — the row for 10.99.0.3/32 flips charlie → bob → charlie.
#
# Runs inside nylon-demo-alice. A failed query prints "—" instead of killing the pane.

TARGET_PREFIX="${TARGET_PREFIX:-10.99.0.3/32}"
TARGETS="10.99.0.1/32 10.99.0.2/32 10.99.0.3/32"

RESET=$'\033[0m'
BOLD=$'\033[1m'
DIM=$'\033[90m'
GREEN=$'\033[1;32m'
YELLOW=$'\033[1;33m'

nh() { # <prefix> — next hop per the daemon's own table
  nylon status --json -i nylon0 2>/dev/null |
    jq -r --arg p "$1" '.status.routes.forward[]? | select(.prefix==$p) | .nh' 2>/dev/null | head -1
}

while true; do
  printf '\033[2J\033[H'
  echo ""
  printf '  %snext hop (nylon0)%s\n' "$DIM" "$RESET"
  echo ""
  for prefix in $TARGETS; do
    hop="$(nh "$prefix")"
    [ -n "$hop" ] || hop="—"
    color=""
    if [ "$prefix" = "$TARGET_PREFIX" ]; then
      case "$hop" in
      charlie) color="$GREEN" ;;
      bob) color="$YELLOW" ;;
      esac
    fi
    printf '  %s%-14s → %s%s\n' "$color" "$prefix" "$hop" "$RESET"
  done
  echo ""
  printf '  %salice .1 · bob .2 · charlie .3%s\n' "$DIM" "$RESET"
  sleep 1
done
