#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COORDINATOR_ADDR="${COORDINATOR_ADDR:-:8020}"
COORDINATOR_HOST="${COORDINATOR_HOST:-127.0.0.1:8020}"
HUB_ADDR="${HUB_ADDR:-127.0.0.1:55355}"
ROOM="${ROOM:-mkds-demo}"
RUNTIME_DIR="${RUNTIME_DIR:-$ROOT/.runtime/mkds-lan}"
PLAYER_COUNT="${PLAYER_COUNT:-4}"
WORKER1_ADDR="${WORKER1_ADDR:-:9021}"
WORKER2_ADDR="${WORKER2_ADDR:-:9022}"
WORKER3_ADDR="${WORKER3_ADDR:-:9023}"
WORKER4_ADDR="${WORKER4_ADDR:-:9024}"
PUBLIC_ADDRESS="${PUBLIC_ADDRESS:-}"
ICE_IP_MAP="${ICE_IP_MAP:-}"
INCLUDE_LOOPBACK="${INCLUDE_LOOPBACK:-false}"

if (( PLAYER_COUNT < 1 || PLAYER_COUNT > 4 )); then
    echo "PLAYER_COUNT must be between 1 and 4" >&2
    exit 1
fi

mkdir -p "$RUNTIME_DIR/logs"

if [[ "${NO_BUILD:-0}" != "1" ]]; then
    GO_BIN="${GO_BIN:-$(command -v go || true)}"
    if [[ -z "$GO_BIN" && -x /usr/local/go/bin/go ]]; then
        GO_BIN=/usr/local/go/bin/go
    fi
    if [[ -z "$GO_BIN" ]]; then
        echo "go binary not found; set GO_BIN=/path/to/go or run with NO_BUILD=1 after building" >&2
        exit 1
    fi
    mkdir -p bin
    "$GO_BIN" build -o bin/ ./cmd/coordinator ./cmd/worker ./cmd/melonds-netpacket-hub
fi

pids=()

cleanup() {
    for pid in "${pids[@]:-}"; do
        kill "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

start() {
    local name="$1"
    shift
    "$@" >"$RUNTIME_DIR/logs/$name.log" 2>&1 &
    local pid=$!
    pids+=("$pid")
    printf "%-12s pid=%s log=%s\n" "$name" "$pid" "$RUNTIME_DIR/logs/$name.log"
}

start hub ./bin/melonds-netpacket-hub -address "$HUB_ADDR"

start coordinator env \
    CLOUD_GAME_COORDINATOR_DEBUG=true \
    CLOUD_GAME_COORDINATOR_SERVER_CACHECONTROL=no-store \
    ./bin/coordinator -address "$COORDINATOR_ADDR"

sleep 1

worker_env=(
    CLOUD_GAME_WORKER_DEBUG=true
    CLOUD_GAME_WEBRTC_INCLUDELOOPBACKCANDIDATE="$INCLUDE_LOOPBACK"
)

if [[ -n "$PUBLIC_ADDRESS" ]]; then
    worker_env+=("CLOUD_GAME_WORKER_NETWORK_PUBLICADDRESS=$PUBLIC_ADDRESS")
fi
if [[ -n "$ICE_IP_MAP" ]]; then
    worker_env+=("CLOUD_GAME_WEBRTC_ICEIPMAP=$ICE_IP_MAP")
fi

for slot in $(seq 1 "$PLAYER_COUNT"); do
    addr_var="WORKER${slot}_ADDR"
    worker_addr="${!addr_var}"
    start "worker-p${slot}" env \
        "${worker_env[@]}" \
        CLOUD_GAME_WORKER_TAG="mkds-p${slot}" \
        CLOUD_GAME_EMULATOR_STORAGE="$RUNTIME_DIR/p${slot}/save" \
        CLOUD_GAME_EMULATOR_LOCALPATH="$RUNTIME_DIR/p${slot}/libretro" \
        MELONDS_NETPLAY_HUB="$HUB_ADDR" \
        MELONDS_NETPLAY_ROOM="$ROOM" \
        MELONDS_NETPLAY_CLIENT_ID="$slot" \
        ./bin/worker -address "$worker_addr" -monitoring.port "$((6620 + slot))" -coordinatorhost "$COORDINATOR_HOST" -zone "mkds-p${slot}"
done

cat <<EOF

Mario Kart DS LAN demo is starting with ${PLAYER_COUNT} player worker(s).

Open:
  http://fedora${COORDINATOR_ADDR}/mkds-lan.html?room=${ROOM}&players=${PLAYER_COUNT}

If remote WebRTC ICE fails over Tailscale, restart with:
  PUBLIC_ADDRESS=fedora ICE_IP_MAP=<tailscale-ip> $0

Press Ctrl-C to stop all demo processes.
EOF

wait
