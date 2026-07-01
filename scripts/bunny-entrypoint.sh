#!/usr/bin/env bash
set -euo pipefail

APP_DIR="${APP_DIR:-/usr/local/share/cloud-game}"
HTTP_PORT="${PORT:-${COORDINATOR_PORT:-8000}}"
PLAYER_COUNT="${PLAYER_COUNT:-4}"
ROOM="${ROOM:-tetris-demo}"
HUB_ADDR="${HUB_ADDR:-127.0.0.1:55355}"
COORDINATOR_HOST="${COORDINATOR_HOST:-127.0.0.1:${HTTP_PORT}}"
XVFB_DISPLAY="${DISPLAY:-:99}"
XVFB_SCREEN="${XVFB_SCREEN:-1280x960x24}"
WEBRTC_BASE_PORT="${WEBRTC_BASE_PORT:-8640}"
WORKER_BASE_PORT="${WORKER_BASE_PORT:-9000}"
MONITORING_BASE_PORT="${MONITORING_BASE_PORT:-6620}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/cloud-game/nds-lan}"
GAME="${GAME:-Tetris-DS-(USA)}"

if (( PLAYER_COUNT < 1 || PLAYER_COUNT > 4 )); then
    echo "PLAYER_COUNT must be between 1 and 4" >&2
    exit 1
fi

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
    ./coordinator -address ":${HTTP_PORT}"

for slot in $(seq 1 "$PLAYER_COUNT"); do
    worker_port=$((WORKER_BASE_PORT + slot))
    monitoring_port=$((MONITORING_BASE_PORT + slot))
    webrtc_port=$((WEBRTC_BASE_PORT + slot))
    save_dir="${RUNTIME_DIR}/p${slot}/save"
    local_dir="${RUNTIME_DIR}/p${slot}/libretro"
    netplay_client_id=$((slot - 1))
    printf -v mac_address '00:08:BF:00:00:%02X' "$slot"

    start_env "worker-p${slot}" \
        DISPLAY="$XVFB_DISPLAY" \
        MESA_GL_VERSION_OVERRIDE="$MESA_GL_VERSION_OVERRIDE" \
        CLOUD_GAME_WORKER_DEBUG="${CLOUD_GAME_WORKER_DEBUG:-true}" \
        CLOUD_GAME_WORKER_TAG="mkds-p${slot}" \
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
        LIBRETRO_USERNAME="mkds-p${slot}" \
        ./worker -address ":${worker_port}" -monitoring.port "$monitoring_port" -coordinatorhost "$COORDINATOR_HOST" -zone "mkds-p${slot}"
done

cat <<EOF

NDS LAN test is running.

Open:
  /mkds-lan.html?room=${ROOM}&players=${PLAYER_COUNT}&game=${GAME}

Expose HTTP port ${HTTP_PORT} and UDP ports $((WEBRTC_BASE_PORT + 1))-$((WEBRTC_BASE_PORT + PLAYER_COUNT)).
Set CLOUD_GAME_WEBRTC_ICEIPMAP or BUNNY_ANYCAST_IP to the public Anycast IP for remote WebRTC.
EOF

set +e
wait -n "${pids[@]}"
exit_code=$?
set -e
echo "a service exited with code ${exit_code}; stopping the app"
exit "$exit_code"
