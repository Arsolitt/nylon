#!/bin/bash
# Brings up the 3-node nylon stand and leaves it running.
#
#   ./up.sh
#
# Generates configs if needed, starts the compose stand, and blocks until alice
# really reaches charlie through the mesh route table. Exits non-zero (with the
# daemon logs) if that does not happen within 120s.
#
# Usage: ./up.sh

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_cmd docker

"$DEMO_DIR/generate-configs.sh"
load_env || die "$ENV_FILE was not written — cannot start the stand"

info "starting the mesh"
dc up -d --force-recreate # always start from the configs on disk

wait_for_shaping 30
warm_up 120

info "next hop toward $TARGET_PREFIX:"
for node in $NODES; do
  hop="$(nh "$node")"
  printf '    %-8s → %s\n' "$node" "${hop:-(none)}"
done

cat <<EOF

  try it:
    docker exec -i $(container alice) ping -c3 $(tunnel_ip charlie)
    docker exec -i $(container alice) nylon status -i nylon0
    docker exec -i $(container charlie) fping -l $(tunnel_ip alice)

  tear it down:
    docker compose --env-file demo/.env -f demo/docker-compose.yml -p nylon-demo down --remove-orphans

EOF
