# NDS Cloud Streaming Runbook

## Bunny container

Use the latest pushed image digest from the release you intend to test. Do not deploy a floating tag unless the platform records the resolved digest.

```text
ghcr.io/rebitplay/cloud-game@sha256:722c910288f66fce7a4e6a69b64e87e9cfe2019640af18253afb33f5f1f9af94
```

Equivalent pushed tags are only for discovery; the Bunny deployment should be pinned to the digest that serves the expected `/buildz` version and WebRTC asset.

After redeploy, verify the running image with authenticated `/buildz`; stale deployments commonly show `webrtc_asset:"webrtc.js?v=9"`. A stale demo page is also easy to spot: current `mkds-lan.html` has API key and ROM SHA1 fields and posts to `/v1/rooms`; if the deployed page posts to `/api/nds/rooms`, Bunny is still serving an older image.

Required public ports:

```text
TCP 8000
UDP 8641
```

Core environment:

```text
HTTP_ADDRESS=:8000
NDS_API_KEY=<shared Rebit -> cloud-game API key>
NDS_TOKEN_SECRET=<long random HS256 secret>
NDS_PUBLIC_ENDPOINT=http://109.224.230.118:8000
NDS_DOWNLOAD_ALLOWED_HOSTS=<Rebit/Bunny ROM and save host allowlist>
NDS_WEBHOOK_URL=https://<rebit>/api/nds-cloud/webhook
NDS_WEBHOOK_SECRET=<shared cloud-game -> Rebit webhook secret>
NDS_PUBLIC_METRICS=true
CLOUD_GAME_COORDINATOR_ORIGIN_USERWS=*
RUNTIME_DIR=/tmp/cloud-game/nds-lan
WEBRTC_MUX_ENABLED=true
WEBRTC_PUBLIC_IP=109.224.230.118
WEBRTC_PUBLIC_PORT=8641
WEBRTC_MUX_LISTEN_ADDR=0.0.0.0:8641
NDS_TURN_URLS=turn:<turn-host>:3478?transport=udp,turn:<turn-host>:3478?transport=tcp
NDS_TURN_SECRET=<turn-rest-secret>
```

Firefox is the browser most likely to expose ICE ordering or TURN problems. Confirm the served frontend uses `web/js/network/webrtc.js?v=10` or newer if using the built-in demo pages. Firefox should be treated as TURN-required on Bunny; Chrome/Edge can use the public UDP mux directly. `CLOUD_GAME_COORDINATOR_ORIGIN_USERWS=*` is required when the player UI is served by Rebit or another host; room tokens still authenticate the user signaling WebSocket.

Firefox failure is not a wait-time issue once the console reports `WebRTC: ICE failed`. If `/buildz` reports `nds_rest_turn_configured:false` and `config_turn_count:0`, Firefox is expected to fail on Bunny even when Chrome works. Add TURN env, redeploy, then retest after `/buildz` changes to `nds_rest_turn_configured:true` or `config_turn_count` is non-zero.

`/api/nds/rooms` is not registered. The `mkds-lan.html` demo page uses the authenticated `/v1/rooms` API directly.

## Health checks

Run the bundled readiness verifier after every Bunny redeploy:

```bash
NDS_ENDPOINT=http://109.224.230.118:8000 \
NDS_PUBLIC_PREFLIGHT_ONLY=true \
node scripts/verify-bunny-nds.mjs
```

The public preflight does not require `NDS_API_KEY`. It checks that the deployed image exposes the current routes and frontend assets. A stale Bunny deployment fails with `/buildz` returning `404`, `mkds-lan.html` still posting to `/api/nds/rooms`, or `network.js` importing `webrtc.js?v=9`.

Then run the authenticated verifier:

```bash
NDS_ENDPOINT=http://109.224.230.118:8000 \
NDS_API_KEY="$NDS_API_KEY" \
NDS_EXPECT_VERSION=8cb0470e-m4-fps-20260704042317 \
NDS_EXPECT_PUBLIC_IP=109.224.230.118 \
NDS_EXPECT_PUBLIC_PORT=8641 \
NDS_REQUIRE_TURN=true \
NDS_REQUIRE_METRICS=true \
NDS_ROOMS=2 \
NDS_PLAYERS=4 \
node scripts/verify-bunny-nds.mjs
```

For Firefox support on Bunny this script must pass. It fails on stale frontend assets, stale demo API paths, missing TURN configuration, missing metrics, or insufficient lazy-spawn capacity.

```bash
curl -fsS http://109.224.230.118:8000/healthz
curl -fsS -H "Authorization: Bearer $NDS_API_KEY" \
  http://109.224.230.118:8000/buildz
curl -fsS -H "Authorization: Bearer $NDS_API_KEY" \
  http://109.224.230.118:8000/v1/capacity
```

`/buildz` should report `webrtc_asset:"webrtc.js?v=10"` or newer, `webrtc_firefox_relay_policy:true`, `webrtc_mux_enabled:true`, `webrtc_public_ip:"109.224.230.118"`, `webrtc_public_port:"8641"`, and `metrics_enabled:true`. For Firefox on Bunny, it should also report `nds_rest_turn_configured:true` or have a non-zero `config_turn_count`.

Create-room failures should be machine-readable JSON. A healthy but full container returns `503 no_capacity`.

## Rebit configuration

Rebit provisions rooms server-to-server and the browser connects directly to the room endpoint returned by cloud-game. Configure the same API and webhook secrets on both sides:

```text
NDS_CLOUD_REGION=sg
NDS_CLOUD_ENDPOINT=http://109.224.230.118:8000
NDS_CLOUD_SG_ENDPOINT=http://109.224.230.118:8000
NDS_CLOUD_US_ENDPOINT=
NDS_CLOUD_EU_ENDPOINT=
NDS_CLOUD_API_KEY=<same value as cloud-game NDS_API_KEY>
NDS_CLOUD_WEBHOOK_SECRET=<same value as cloud-game NDS_WEBHOOK_SECRET>
NDS_CLOUD_TIMEOUT=30
APP_URL=https://<public-rebit-host>
```

`config/services.php` maps these into `services.nds_cloud`. Rebit first tries the requested/default region, then falls through the remaining configured endpoints only when cloud-game returns `503 {"code":"no_capacity"}`. The saved `nds_cloud_sessions.endpoint` is the exact container endpoint used for later token refresh and close requests.

`APP_URL` must be publicly reachable from Bunny because Rebit signs per-seat save PUT URLs under `/api/nds-cloud/sessions/{room}/players/{player}/save`. Add that host, plus the ROM CDN host, to `NDS_DOWNLOAD_ALLOWED_HOSTS` on Bunny. Do not template a signed save URL after signing; Rebit signs the final room/player path for each seat.

The webhook endpoint is:

```text
POST /api/nds-cloud/webhook
```

It requires `X-NDS-Delivery`, `X-NDS-Timestamp`, and `X-NDS-Signature: sha256=<hmac>`, dedupes deliveries, updates the `nds_cloud_sessions` row, registers `save.uploaded` payloads as `GameSave` versions, and broadcasts lobby state changes.

## Metrics

Enable the existing monitoring server with the config/env used by the deployment. The Prometheus endpoint is `/metrics`.

NDS-specific metrics:

```text
cloud_game_nds_rooms{state}
cloud_game_nds_seats_connected
cloud_game_nds_room_create_duration_seconds{status}
cloud_game_nds_token_failures_total{reason}
cloud_game_nds_webhook_retries_total{event}
cloud_game_nds_save_upload_failures_total{stage}
cloud_game_nds_netpacket_packets_total{room,player,direction}
cloud_game_worker_video_fps{worker}
cloud_game_worker_video_frames_total{worker}
cloud_game_worker_video_encode_duration_seconds{worker}
```

Useful checks:

```promql
sum(cloud_game_nds_rooms{state="active"})
sum(cloud_game_nds_seats_connected)
histogram_quantile(0.95, sum(rate(cloud_game_nds_room_create_duration_seconds_bucket[5m])) by (vmrange))
sum(rate(cloud_game_nds_webhook_retries_total[5m])) by (event)
sum(rate(cloud_game_nds_save_upload_failures_total[5m])) by (stage)
sum(cloud_game_worker_video_fps) by (worker)
sum(rate(cloud_game_worker_video_frames_total[1m])) by (worker)
```

Before running the M4 load proof, confirm the deployed container exposes metrics:

```bash
curl -fsS -H "Authorization: Bearer $NDS_API_KEY" \
  http://109.224.230.118:8000/metrics | grep cloud_game_nds_rooms
```

If this returns `404`, the container is missing `NDS_PUBLIC_METRICS=true` or is still running an image older than the coordinator metrics route. If it returns `401`, the API key does not match the deployed `NDS_API_KEY`.

Audit logs are emitted as structured log lines:

```text
audit=nds_room_create room=<room> refs=[...] create_latency=<duration>
audit=nds_room_close room=<room> refs=[...] duration_sec=<n> reason=<reason>
```

`bytes_streamed` is counted from encoded audio/video samples sent by workers to WebRTC recipients and folded into the final room close audit.

## Load test

The primary load harness lives in `../rebit` and imports the real `@rebit/nds-stream` SDK into headless Playwright clients. It provisions rooms through `/v1/rooms`, connects each seat through the SDK, and exits non-zero when the configured capacity/join/RTT thresholds fail.

Install browser dependencies in `../rebit`:

```bash
pnpm install
pnpm exec playwright install chromium
```

Run a 2-room, 4-player capacity test:

```bash
cd ../rebit
NDS_SAVE_UPLOAD_LISTEN=0.0.0.0:18080 \
NDS_SAVE_UPLOAD_PUBLIC_BASE_URL=http://<load-runner-public-host>:18080 \
NDS_ENDPOINT=http://109.224.230.118:8000 \
NDS_API_KEY="$NDS_API_KEY" \
NDS_ROM_URL="https://<cdn>/Tetris-DS-USA.nds" \
NDS_ROM_SHA1="<40-char-sha1>" \
NDS_ROM_NAME="Tetris-DS-USA.nds" \
NDS_ROOMS=2 \
NDS_PLAYERS=4 \
NDS_DURATION_SEC=1800 \
NDS_JOIN_P95_MAX_MS=10000 \
NDS_RTT_P95_MAX_MS=200 \
NDS_METRICS_URL=http://109.224.230.118:8000/metrics \
NDS_CLOSE_SETTLE_MS=10000 \
NDS_SAVE_UPLOAD_FAILURE_MAX=0 \
NDS_WEBHOOK_RETRY_MAX=0 \
NDS_REQUIRE_CAPACITY_RECOVERY=true \
NDS_REQUIRE_METRICS_RECOVERY=true \
NDS_REQUIRE_WORKER_VIDEO_METRICS=true \
NDS_REQUIRE_NETPACKET_METRICS=false \
pnpm load:nds-cloud
```

The load runner starts a temporary save-upload sink and gives cloud-game per-seat `save_upload_url` values under `NDS_SAVE_UPLOAD_PUBLIC_BASE_URL`. The Bunny container must be able to reach that URL. For local non-container smoke tests you may use `NDS_SAVE_UPLOAD_PUBLIC_BASE_URL=http://127.0.0.1` with the default random listen port. For containerized local smoke tests, use a host-reachable address such as `http://host.containers.internal:<port>`, set `NDS_SAVE_UPLOAD_LISTEN=0.0.0.0:<port>`, and run the local cloud-game container with `NDS_DOWNLOAD_ALLOWED_HOSTS=host.containers.internal` plus `NDS_ALLOW_PRIVATE_REMOTE_URLS=true`. Do not use `NDS_ALLOW_PRIVATE_REMOTE_URLS=true` on Bunny or production; remote Bunny runs should use an explicit public reachable address and port as shown above.

For local packaged-image smoke tests, the bundled ROM can be used without a CDN:

```bash
NDS_ROM_URL='builtin:Tetris-DS-(USA).nds'
NDS_ROM_SHA1=13eb2e7e5357a6e31f94ea238826c111c965bc9b
```

`NDS_ROM_NAME` is optional when the file name can be inferred from `NDS_ROM_URL`; set it explicitly for signed URLs whose path does not end in the original `.nds` name.

Do not use a templated Laravel signed URL such as `/sessions/{room}/players/{player}/save?...` unless each final room/player URL was signed after substitution; changing path parameters after signing invalidates the signature.

The script preflights `/healthz`, authenticated `/buildz`, and authenticated `/v1/capacity`, then prints JSON with health/build/capacity snapshots, join-time, RTT, FPS, final inbound video stats, SDK state events, save-upload sink results, active/before/after metrics snapshots, metrics deltas, and threshold results. It fails before opening browsers when the deployed build is stale, Firefox/TURN prerequisites are missing, or `by_players[NDS_PLAYERS]`/`total_rooms` is below `NDS_ROOMS`. By default it also fails when capacity or connected-seat metrics do not recover after cleanup; set `NDS_REQUIRE_CAPACITY_RECOVERY=false` and `NDS_REQUIRE_METRICS_RECOVERY=false` only for shared-container smoke tests. When `NDS_METRICS_URL` is set it checks that active rooms/seats appear during the run, worker video frames increase, waits `NDS_CLOSE_SETTLE_MS` after cleanup, scrapes metrics with `Authorization: Bearer $NDS_API_KEY`, then fails the run if save-upload failures or webhook retries increase beyond the configured max values. Set `NDS_REQUIRE_NETPACKET_METRICS=true` for a multiplayer-flow proof where the ROM is expected to generate melonDS LAN packet counters. For M4 acceptance, run it on the target 16-vCPU Bunny container for 30 minutes and compare:

```text
Join time p95 <= 10s with cached ROM
Same-region RTT should stay comfortably below the NR-1 latency budget
2 concurrent 4-player rooms stay connected for the full run
No save upload failures or webhook retry spikes
Worker video FPS stays positive while rooms are active
Worker video frame metrics increase while rooms are active
```

Glass-to-glass and input-to-photon still need an external visual timing rig or browser instrumentation beyond WebRTC RTT; do not claim NR-1 is fully proven from RTT alone.

`cloud-game/scripts/nds-load-test.mjs` is kept as a direct protocol smoke harness for service debugging, but it should not be used as the final M4 SDK proof.

## Firefox ICE failure checklist

If Firefox reports `WebRTC: ICE failed`:

1. Confirm Bunny is running the digest above, not an older cached tag: `curl -fsS -H "Authorization: Bearer $NDS_API_KEY" http://109.224.230.118:8000/buildz`.
2. If `/buildz` shows `nds_rest_turn_configured:false` and `config_turn_count:0`, configure `NDS_TURN_URLS` and `NDS_TURN_SECRET`; Firefox should not be treated as supported on Bunny until that is true.
3. Confirm UDP 8641 is exposed and mapped to the container for Chrome/Edge direct mux tests.
4. Confirm `WEBRTC_MUX_ENABLED=true`, `WEBRTC_PUBLIC_IP=109.224.230.118`, and `WEBRTC_PUBLIC_PORT=8641`.
5. In Firefox `about:webrtc`, confirm the selected candidate pair uses `relay` after TURN is configured.
6. Confirm TURN credentials are present in `INIT` and relay candidates are not rewritten by the mux.
7. Check `cloud_game_nds_token_failures_total` and container logs for token/session mismatches.

## Shutdown and saves

Rooms close on host delete, idle timeout, join timeout, max duration, drain, or error. On graceful close the coordinator asks each worker to flush SRAM and emits `room.closed`. Rebit registers uploaded saves from the signed upload path when it receives `save.uploaded`.

During deploy or drain, prefer a graceful stop window of at least 60 seconds:

```text
NDS_DRAIN_TIMEOUT=60s
```

If the container restarts with a room journal present, it emits `room.closed` with `reason:"error"` for orphaned rooms on boot.
