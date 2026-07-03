#!/usr/bin/env bash
set -euo pipefail

APP_DIR="${APP_DIR:-/usr/local/share/cloud-game}"
HTTP_PORT="${PORT:-${COORDINATOR_PORT:-8000}}"
ROOM_COUNT="${ROOM_COUNT:-${NDS_ROOM_COUNT:-0}}"
NDS_MAX_ROOM_COUNT="${NDS_MAX_ROOM_COUNT:-8}"
NDS_AUTO_SPAWN="${NDS_AUTO_SPAWN:-true}"
PLAYER_COUNT="${PLAYER_COUNT:-${NDS_PLAYERS_PER_ROOM:-4}}"
ROOM="${ROOM:-tetris-demo}"
HUB_ADDR="${HUB_ADDR:-127.0.0.1:55355}"
COORDINATOR_HOST="${COORDINATOR_HOST:-127.0.0.1:${HTTP_PORT}}"
XVFB_DISPLAY="${DISPLAY:-:99}"
XVFB_SCREEN="${XVFB_SCREEN:-1280x960x24}"
WEBRTC_MUX_ENABLED="${WEBRTC_MUX_ENABLED:-true}"
WEBRTC_PUBLIC_PORT="${WEBRTC_PUBLIC_PORT:-8641}"
if [[ "${WEBRTC_MUX_ENABLED,,}" == "true" || "${WEBRTC_MUX_ENABLED}" == "1" || "${WEBRTC_MUX_ENABLED,,}" == "yes" || "${WEBRTC_MUX_ENABLED,,}" == "on" ]]; then
    WEBRTC_BASE_PORT="${WEBRTC_WORKER_BASE_PORT:-${WEBRTC_BASE_PORT:-8700}}"
else
    WEBRTC_BASE_PORT="${WEBRTC_WORKER_BASE_PORT:-${WEBRTC_BASE_PORT:-8640}}"
fi
WORKER_BASE_PORT="${WORKER_BASE_PORT:-9000}"
MONITORING_BASE_PORT="${MONITORING_BASE_PORT:-6620}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/cloud-game/nds-lan}"
GAME="${GAME:-Tetris-DS-(USA)}"

if (( ROOM_COUNT < 1 )); then
    if (( ROOM_COUNT != 0 )); then
        echo "ROOM_COUNT must be 0 or greater" >&2
        exit 1
    fi
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

cd "$APP_DIR"
mkdir -p "$RUNTIME_DIR"

pids=()

stop_all() {
    trap - EXIT INT TERM
    for pid in "${pids[@]:-}"; do
        kill "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
}
trap stop_all EXIT INT TERM

start() {
    local name="$1"
    shift
    echo "starting ${name}: $*"
    "$@" &
    pids+=("$!")
}

start_env() {
    local name="$1"
    shift
    echo "starting ${name}: $*"
    env "$@" &
    pids+=("$!")
}

export DISPLAY="$XVFB_DISPLAY"
export MESA_GL_VERSION_OVERRIDE="${MESA_GL_VERSION_OVERRIDE:-4.5}"

start xvfb Xvfb "$XVFB_DISPLAY" -screen 0 "$XVFB_SCREEN" -nolisten tcp

for _ in {1..100}; do
    if [ -S "/tmp/.X11-unix/X${XVFB_DISPLAY#:}" ]; then
        break
    fi
    sleep 0.1
done

start hub ./melonds-netpacket-hub -address "$HUB_ADDR"

start_env coordinator \
    CLOUD_GAME_COORDINATOR_DEBUG="${CLOUD_GAME_COORDINATOR_DEBUG:-true}" \
    CLOUD_GAME_COORDINATOR_SERVER_CACHECONTROL="${CLOUD_GAME_COORDINATOR_SERVER_CACHECONTROL:-no-store}" \
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
    WEBRTC_PUBLIC_IP="${WEBRTC_PUBLIC_IP:-${CLOUD_GAME_WEBRTC_ICEIPMAP:-${BUNNY_ANYCAST_IP:-}}}" \
    WEBRTC_WORKER_BASE_PORT="$WEBRTC_BASE_PORT" \
    WORKER_BASE_PORT="$WORKER_BASE_PORT" \
    MONITORING_BASE_PORT="$MONITORING_BASE_PORT" \
    BUNNY_ANYCAST_IP="${BUNNY_ANYCAST_IP:-}" \
    ./coordinator -address ":${HTTP_PORT}"

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
            worker_port=$((WORKER_BASE_PORT + global_slot))
            monitoring_port=$((MONITORING_BASE_PORT + global_slot))
            webrtc_port=$((WEBRTC_BASE_PORT + global_slot))
            save_dir="${RUNTIME_DIR}/${group_name}/p${player}/save"
            local_dir="${RUNTIME_DIR}/${group_name}/p${player}/libretro"
            netplay_client_id=$((player - 1))
            zone="${group_name}-p${player}"
            printf -v mac_address '00:08:BF:%02X:00:%02X' "$group" "$player"

            start_env "worker-${zone}" \
                DISPLAY="$XVFB_DISPLAY" \
                MESA_GL_VERSION_OVERRIDE="$MESA_GL_VERSION_OVERRIDE" \
                CLOUD_GAME_WORKER_DEBUG="${CLOUD_GAME_WORKER_DEBUG:-true}" \
                CLOUD_GAME_WORKER_TAG="$zone" \
                CLOUD_GAME_WORKER_NDS_GROUP="$group_name" \
                CLOUD_GAME_WORKER_NDS_PLAYER="$player" \
                CLOUD_GAME_ENCODER_VIDEO_CODEC="${CLOUD_GAME_ENCODER_VIDEO_CODEC:-vp8}" \
                CLOUD_GAME_WEBRTC_SINGLEPORT="$webrtc_port" \
                CLOUD_GAME_WEBRTC_INCLUDELOOPBACKCANDIDATE="${CLOUD_GAME_WEBRTC_INCLUDELOOPBACKCANDIDATE:-false}" \
                CLOUD_GAME_WEBRTC_ICEIPMAP="${CLOUD_GAME_WEBRTC_ICEIPMAP:-${BUNNY_ANYCAST_IP:-}}" \
                CLOUD_GAME_EMULATOR_STORAGE="$save_dir" \
                CLOUD_GAME_EMULATOR_LOCALPATH="$local_dir" \
                MELONDS_NETPLAY_HUB="$HUB_ADDR" \
                MELONDS_NETPLAY_ROOM="$ROOM" \
                MELONDS_NETPLAY_CLIENT_ID="$netplay_client_id" \
                MELONDS_MAC_ADDRESS="$mac_address" \
                LIBRETRO_USERNAME="$zone" \
                ./worker -address ":${worker_port}" -monitoring.port "$monitoring_port" -coordinatorhost "$COORDINATOR_HOST" -zone "$zone"
        done
    done
fi

cat <<EOF

NDS LAN test is running.

Open:
  /mkds-lan.html?room=${ROOM}&players=${PLAYER_COUNT}&game=${GAME}

Warm NDS room groups: ${ROOM_COUNT}
Max lazy NDS room groups: ${NDS_MAX_ROOM_COUNT}
Warm NDS workers: ${TOTAL_WORKERS}
Max NDS workers: ${MAX_WORKERS}

EOF

if [[ "${WEBRTC_MUX_ENABLED,,}" == "true" || "${WEBRTC_MUX_ENABLED}" == "1" || "${WEBRTC_MUX_ENABLED,,}" == "yes" || "${WEBRTC_MUX_ENABLED,,}" == "on" ]]; then
    cat <<EOF
Expose HTTP port ${HTTP_PORT} and UDP port ${WEBRTC_PUBLIC_PORT}.
Worker WebRTC ports $((WEBRTC_BASE_PORT + 1))-$((WEBRTC_BASE_PORT + MAX_WORKERS)) stay internal.
EOF
else
    cat <<EOF
Expose HTTP port ${HTTP_PORT} and UDP ports $((WEBRTC_BASE_PORT + 1))-$((WEBRTC_BASE_PORT + MAX_WORKERS)).
EOF
fi

cat <<EOF
Set CLOUD_GAME_WEBRTC_ICEIPMAP or BUNNY_ANYCAST_IP to the public Anycast IP for remote WebRTC.
For Firefox or strict NAT fallback, set WEBRTC_TURN_URLS, WEBRTC_TURN_USERNAME, and WEBRTC_TURN_CREDENTIAL.
EOF

set +e
wait -n "${pids[@]}"
exit_code=$?
set -e
echo "a service exited with code ${exit_code}; stopping the app"
exit "$exit_code"
