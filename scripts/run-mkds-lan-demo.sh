#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COORDINATOR_ADDR="${COORDINATOR_ADDR:-:8020}"
COORDINATOR_HOST="${COORDINATOR_HOST:-127.0.0.1:8020}"
HUB_ADDR="${HUB_ADDR:-127.0.0.1:55355}"
ROOM="${ROOM:-mkds-demo}"
GAME="${GAME:-Mario-Kart-DS-USA}"
RUNTIME_DIR="${RUNTIME_DIR:-$ROOT/.runtime/mkds-lan}"
ROOM_COUNT="${ROOM_COUNT:-${NDS_ROOM_COUNT:-0}}"
NDS_MAX_ROOM_COUNT="${NDS_MAX_ROOM_COUNT:-8}"
NDS_AUTO_SPAWN="${NDS_AUTO_SPAWN:-true}"
PLAYER_COUNT="${PLAYER_COUNT:-${NDS_PLAYERS_PER_ROOM:-4}}"
WORKER_BASE_PORT="${WORKER_BASE_PORT:-9020}"
WEBRTC_MUX_ENABLED="${WEBRTC_MUX_ENABLED:-true}"
WEBRTC_PUBLIC_PORT="${WEBRTC_PUBLIC_PORT:-8641}"
if [[ "${WEBRTC_MUX_ENABLED,,}" == "true" || "${WEBRTC_MUX_ENABLED}" == "1" || "${WEBRTC_MUX_ENABLED,,}" == "yes" || "${WEBRTC_MUX_ENABLED,,}" == "on" ]]; then
    WEBRTC_BASE_PORT="${WEBRTC_WORKER_BASE_PORT:-${WEBRTC_BASE_PORT:-8720}}"
else
    WEBRTC_BASE_PORT="${WEBRTC_WORKER_BASE_PORT:-${WEBRTC_BASE_PORT:-8640}}"
fi
PUBLIC_ADDRESS="${PUBLIC_ADDRESS:-}"
ICE_IP_MAP="${ICE_IP_MAP:-}"
INCLUDE_LOOPBACK="${INCLUDE_LOOPBACK:-false}"

if (( ROOM_COUNT < 0 )); then
    echo "ROOM_COUNT must be 0 or greater" >&2
    exit 1
fi

if (( NDS_MAX_ROOM_COUNT < 1 )); then
    echo "NDS_MAX_ROOM_COUNT must be at least 1" >&2
    exit 1
fi

if (( PLAYER_COUNT < 1 || PLAYER_COUNT > 4 )); then
    echo "PLAYER_COUNT must be between 1 and 4" >&2
    exit 1
fi

if (( ROOM_COUNT > NDS_MAX_ROOM_COUNT )); then
    echo "ROOM_COUNT cannot exceed NDS_MAX_ROOM_COUNT" >&2
    exit 1
fi

TOTAL_WORKERS=$((ROOM_COUNT * PLAYER_COUNT))
MAX_WORKERS=$((NDS_MAX_ROOM_COUNT * PLAYER_COUNT))

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
    COORDINATOR_HOST="$COORDINATOR_HOST" \
    HUB_ADDR="$HUB_ADDR" \
    NDS_AUTO_SPAWN="$NDS_AUTO_SPAWN" \
    NDS_MAX_ROOM_COUNT="$NDS_MAX_ROOM_COUNT" \
    NDS_PLAYERS_PER_ROOM="$PLAYER_COUNT" \
    ROOM_COUNT="$ROOM_COUNT" \
    RUNTIME_DIR="$RUNTIME_DIR" \
    WEBRTC_BASE_PORT="$WEBRTC_BASE_PORT" \
    WEBRTC_MUX_ENABLED="$WEBRTC_MUX_ENABLED" \
    WEBRTC_PUBLIC_PORT="$WEBRTC_PUBLIC_PORT" \
    WEBRTC_PUBLIC_IP="${WEBRTC_PUBLIC_IP:-$ICE_IP_MAP}" \
    WEBRTC_WORKER_BASE_PORT="$WEBRTC_BASE_PORT" \
    WORKER_BASE_PORT="$WORKER_BASE_PORT" \
    MONITORING_BASE_PORT=6620 \
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

global_slot=0
if (( ROOM_COUNT > 0 )); then
    for group in $(seq 1 "$ROOM_COUNT"); do
        if (( ROOM_COUNT == 1 )); then
            group_name="mkds"
        else
            group_name="mkds-r${group}"
        fi

        for player in $(seq 1 "$PLAYER_COUNT"); do
            global_slot=$((global_slot + 1))
            worker_addr=":$((WORKER_BASE_PORT + global_slot))"
            netplay_client_id=$((player - 1))
            zone="${group_name}-p${player}"
            printf -v mac_address '00:08:BF:%02X:00:%02X' "$group" "$player"
            start "worker-${zone}" env \
                "${worker_env[@]}" \
                CLOUD_GAME_WORKER_TAG="$zone" \
                CLOUD_GAME_WORKER_NDS_GROUP="$group_name" \
                CLOUD_GAME_WORKER_NDS_PLAYER="$player" \
                CLOUD_GAME_EMULATOR_STORAGE="$RUNTIME_DIR/${group_name}/p${player}/save" \
                CLOUD_GAME_EMULATOR_LOCALPATH="$RUNTIME_DIR/${group_name}/p${player}/libretro" \
                MELONDS_NETPLAY_HUB="$HUB_ADDR" \
                MELONDS_NETPLAY_ROOM="$ROOM" \
                MELONDS_NETPLAY_CLIENT_ID="$netplay_client_id" \
                MELONDS_MAC_ADDRESS="$mac_address" \
                LIBRETRO_USERNAME="$zone" \
                ./bin/worker -address "$worker_addr" -monitoring.port "$((6620 + global_slot))" -coordinatorhost "$COORDINATOR_HOST" -zone "$zone"
        done
    done
fi

cat <<EOF

NDS LAN demo is starting with ${ROOM_COUNT} warm room group(s), ${TOTAL_WORKERS} warm worker(s).
Lazy capacity is ${NDS_MAX_ROOM_COUNT} room group(s), ${MAX_WORKERS} worker port(s).
WebRTC mux: ${WEBRTC_MUX_ENABLED}, public UDP port: ${WEBRTC_PUBLIC_PORT}, worker base UDP port: ${WEBRTC_BASE_PORT}.
Firefox/strict NAT fallback: set WEBRTC_TURN_URLS, WEBRTC_TURN_USERNAME, and WEBRTC_TURN_CREDENTIAL.

Open:
  http://fedora${COORDINATOR_ADDR}/mkds-lan.html?room=${ROOM}&players=${PLAYER_COUNT}&game=${GAME}

If remote WebRTC ICE fails over Tailscale, restart with:
  PUBLIC_ADDRESS=fedora ICE_IP_MAP=<tailscale-ip> $0

Press Ctrl-C to stop all demo processes.
EOF

wait
