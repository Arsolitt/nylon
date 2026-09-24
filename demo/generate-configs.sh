#!/bin/bash
# Generates everything the stand needs before `docker compose up`: a free mesh
# subnet (demo/.env), three WireGuard keypairs and the central/node YAML configs
# (demo/configs/), then validates the configs with the real validator.
#
# Idempotent: an existing demo/.env and demo/configs/ are reused as-is. Pass
# --force to regenerate both.
#
# Usage: ./generate-configs.sh [--force]

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

FORCE=0
case "${1:-}" in
"") ;;
--force) FORCE=1 ;;
*) die "usage: $(basename "$0") [--force]" ;;
esac

require_cmd docker

# ── 1. Mesh subnet + demo/.env ───────────────────────────────────────
if [ "$FORCE" = 1 ] || ! load_env; then
  subnet="$(pick_subnet)"
  write_env "$subnet"
  load_env
  info "allocated mesh subnet $MESH_SUBNET (nodes: $ALICE_IP, $BOB_IP, $CHARLIE_IP)"
else
  info "reusing $ENV_FILE (mesh subnet $MESH_SUBNET)"
fi

# ── 2. Image (compose has to render before anything can be built) ────
if docker image inspect "$IMAGE" >/dev/null 2>&1; then
  info "reusing image $IMAGE"
else
  info "building image $IMAGE (first run, this takes a few minutes)"
  dc build
fi

# ── 3. Keys + configs ────────────────────────────────────────────────
if [ "$FORCE" = 1 ] || [ ! -f "$CONFIGS_DIR/central.yaml" ]; then
  info "generating keys and configs into $CONFIGS_DIR"
  scratch="$(mktemp -d)"
  trap 'rm -rf "$scratch"' EXIT

  # nylon key prints the private key to stdout and the public key to stderr.
  gen_key() { # <scratch dir> — sets KEY_PRIVATE / KEY_PUBLIC
    docker run --rm --entrypoint /usr/local/bin/nylon "$IMAGE" key \
      >"$1/private" 2>"$1/public" || die "key generation failed"
    KEY_PRIVATE="$(cat "$1/private")"
    KEY_PUBLIC="$(cat "$1/public")"
  }

  for node in $NODES; do
    gen_key "$scratch"
    eval "${node}_private=\$KEY_PRIVATE"
    eval "${node}_public=\$KEY_PUBLIC"
  done

  mkdir -p "$CONFIGS_DIR"
  {
    echo "routers:"
    for node in $NODES; do
      eval pubkey=\${${node}_public}
      echo "  - id: $node"
      echo "    pubkey: $pubkey"
      echo "    addresses: [$(tunnel_ip "$node")]"
      echo "    endpoints:"
      echo "      - \"$(subnet_host_ip "$MESH_SUBNET" "$(host_octet "$node")"):$PORT\""
    done
    echo ""
    echo "graph:"
    echo "  - alice, bob, charlie"
  } >"$CONFIGS_DIR/central.yaml"

  for node in $NODES; do
    eval private=\${${node}_private}
    {
      echo "id: $node"
      echo "key: $private"
      echo "port: $PORT"
      echo "interface_name: nylon0"
      if [ "$node" = "alice" ]; then
        # Deterministic topology. On a docker bridge every link measures the same
        # sub-millisecond latency with comparable jitter, so the mesh re-selects
        # routes on noise and a "reroute" proves nothing. Shaping alice's egress
        # to bob to 20ms makes the direct alice-charlie path win by ~20x, and the
        # path through bob the only sensible fallback.
        echo "pre_up:"
        echo "  - 'tc qdisc add dev eth0 root handle 1: prio bands 3 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0'"
        echo "  - 'tc qdisc add dev eth0 parent 1:2 handle 20: netem delay ${BOB_LINK_DELAY_MS}ms'"
        echo "  - 'tc filter add dev eth0 parent 1: protocol ip prio 2 u32 match ip dst $(subnet_host_ip "$MESH_SUBNET" "$(host_octet bob)")/32 flowid 1:2'"
      fi
    } >"$CONFIGS_DIR/$node.yaml"
  done
else
  info "configs already present in $CONFIGS_DIR, reusing (pass --force to regenerate)"
fi

# ── 4. Validate with the real validator ──────────────────────────────
for node in $NODES; do
  docker run --rm -v "$CONFIGS_DIR:/cfg:ro" --entrypoint /usr/local/bin/nylon "$IMAGE" \
    verify /cfg/central.yaml --node "/cfg/$node.yaml" >/dev/null ||
    die "$node.yaml failed validation (run: docker run --rm -v $CONFIGS_DIR:/cfg:ro --entrypoint /usr/local/bin/nylon $IMAGE verify /cfg/central.yaml --node /cfg/$node.yaml)"
  info "$node.yaml: valid"
done

# ── 5. Summary ───────────────────────────────────────────────────────
info "mesh subnet: $MESH_SUBNET"
for node in $NODES; do
  info "  $node: tunnel $(tunnel_ip "$node")  docker $(subnet_host_ip "$MESH_SUBNET" "$(host_octet "$node")")"
done
info "configs: $(ls "$CONFIGS_DIR" | tr '\n' ' ')"
