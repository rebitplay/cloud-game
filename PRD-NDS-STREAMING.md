# PRD — NDS Cloud Multiplayer Streaming for Rebit

| | |
|---|---|
| **Status** | Approved for implementation |
| **Version** | 1.0 (2026-07-03) |
| **Repos** | `cloud-game` (this repo, service) · `../rebit` (Laravel 12 + React 19 + Inertia, consumer) |
| **Deployment** | Bunny Magic Containers (per region), Bunny CDN storage, Bunny anycast + TURN |
| **Audience** | Implementation agents. Every requirement is numbered (FR/SR/NR) and has acceptance criteria (§12). |

---

## 1. Overview

Rebit lets users play retro games in the browser (RetroArch WASM / Nostalgist.js) with
their own ROM library and saves. NDS multiplayer (DS local wireless) cannot run in
browser WASM. This product adds **server-side NDS multiplayer**: for each room, the
service runs one melonDS emulator instance **per player** on the same container,
syncs DS local-wireless packets between the instances (libretro `retro_netpacket`
over a local UDP hub), and streams each player's own DS video/audio to their browser
over WebRTC, with input (buttons + touch) sent back on data channels.

Rebit's backend provisions rooms through a server-to-server REST API. **Rebit's React
app connects directly to the room server via WebRTC** — there is **no iframe/embed**;
the stream renders in a native Rebit `<video>` element with Rebit's own touch overlay
and controls. ROMs and saves stay owned by Rebit (Bunny CDN); the service downloads
them at room start and uploads saves back during/after the session.

The POC on branch `rebit` already proves: per-player workers, netpacket hub (RNP1),
room-provisioning HTTP API, dynamic worker spawning, single-UDP-port WebRTC mux for
anycast, TURN fallback, Bunny container entrypoint. This PRD defines what must be
built/changed to ship it.

## 2. Goals / Non-goals

**Goals**
1. Rebit users create an NDS multiplayer room (2–4 players) from the existing lobby
   flow and play together with their own ROMs and saves.
2. Each player streams *their own* DS console (private screen, personal touch input).
3. Saves persist back to Rebit's `GameSave` automatically (periodic + on exit).
4. Only Rebit's backend can provision rooms; only invited players can join a stream.
5. Runs on Bunny Magic Containers with no ops beyond container config.

**Non-goals (v1)**
- Nintendo WFC / internet play between rooms (future; `wfc-handoff` project).
- Savestates sync (SRAM only in v1; API reserves fields).
- Spectator mode (v1.1 — CloudRetro already supports multi-viewer per room).
- Voice chat, game recording.
- Non-NDS systems through this pipeline.

## 3. User stories

- **US-1** As a Rebit user, I open an NDS game, choose "Multiplayer", get a room link,
  and friends join from the lobby (existing Rebit netplay-room UX).
- **US-2** When the host presses Start, within ~10 s every member sees their own DS
  screen streaming inside the normal Rebit game console UI, with Rebit's touch
  overlay/d-pad working (touch is mandatory for DS).
- **US-3** My existing save for that game loads into my DS; when the session ends (or
  I quit), my save appears in my Rebit saves list, updated.
- **US-4** If I refresh the page mid-game, I reconnect to my seat within seconds.
- **US-5** If servers are full, the host sees "servers busy, try again" before anyone
  waits.
- **US-6** When everyone leaves, the room shuts itself down and playtime is recorded.

## 4. System overview

```
 REBIT backend (Laravel) ──REST + API key──►  ROOM SERVER (Bunny Magic Container, per region)
        ▲                                     ┌──────────────────────────────────────────┐
        │  webhooks (HMAC)                    │ coordinator (API, tokens, signaling WS,  │
        │                                     │  WebRTC UDP mux :8641, spawner)          │
 REBIT React app (per player)                 │ worker group per room (on same host):    │
   @rebit/nds-stream SDK ──WS signaling──────►│   worker-P1..P4 (melonDS + encoder)      │
   <video> + touch overlay ◄─WebRTC RTP/DC───►│   local UDP netpacket hub (RNP1)         │
                                              └──────────────────────────────────────────┘
 Bunny CDN storage: ROMs (presigned GET), saves (presigned GET in / presigned PUT out)
 TURN (per region): relay fallback when direct UDP fails
```

**Deployment model (fixed by decision):** one self-contained container image =
coordinator + spawner + workers + WebRTC mux (today's `scripts/bunny-entrypoint.sh`).
Bunny Magic Containers runs one deployment per region. **Room affinity rule:** all
API-returned player URLs must pin to the exact container hosting the room (see FR-7) —
never rely on anycast to route a *specific* room's traffic.

## 5. Functional requirements — service (`cloud-game`)

### FR-1 Room provisioning API (v1, authenticated)

New namespace `/v1`, JSON, auth `Authorization: Bearer <NDS_API_KEY>`
(env, constant-time compare, support 2 keys for rotation). The legacy open
`/api/nds/rooms` endpoint is **removed** (not deprecated — nothing else consumes it).
CORS headers are removed from control-plane endpoints (server-to-server only).

#### FR-1.1 `POST /v1/rooms`

```jsonc
// Request
{
  "room": "rebit-7f3a9c",              // Rebit room id — idempotency key, [a-z0-9-]{4,64}
  "players": 3,                         // 2..4
  "rom": {
    "url": "https://cdn.rebitplay.com/roms/x.nds?token=…",   // presigned GET
    "name": "Mario Kart DS (USA).nds",
    "sha1": "ab12…"                     // REQUIRED; worker verifies after download
  },
  "player_slots": [                     // exactly `players` entries, player 1..players
    {
      "player": 1,
      "ref": "user_5521",               // opaque Rebit user ref, echoed in webhooks
      "save_url": "https://…srm?token=…",          // presigned GET; optional
      "save_upload_url": "https://…srm?sig=…"      // presigned PUT; REQUIRED
    }
  ],
  "options": {                          // all optional
    "video_codec": "h264",              // "h264" | "vp8"; default: container env
    "max_duration_sec": 14400,          // default 14400 (4 h), max 21600
    "idle_timeout_sec": 300,            // all players disconnected this long → close
    "join_timeout_sec": 600             // ready but nobody ever joined → close
  }
}
```

```jsonc
// Response 201 (or 200 when idempotent-replayed)
{
  "room_id": "rebit-7f3a9c",
  "state": "ready",
  "game": "Mario Kart DS (USA)",
  "endpoint": "https://sg-1.nds.rebitplay.com",   // this container's public endpoint
  "join_deadline": "2026-07-03T12:10:00Z",
  "players": [
    {
      "player": 1,
      "ref": "user_5521",
      "token": "<JWT>",                                          // §FR-2
      "signaling_url": "wss://sg-1.nds.rebitplay.com/ws?token=<JWT>",
      "ice_servers": [ {"urls":"stun:…"}, {"urls":"turn:…","username":"…","credential":"…"} ]
    }
  ]
}
```

Semantics:
- **Idempotent on `room`**: same `room` while a session is live → return the existing
  session with **fresh tokens**, `200`. (Replaces today's `409` on re-create; solves
  Rebit retry safety.)
- Provisioning is synchronous up to worker reservation + ROM install trigger; if the
  ROM/saves are still downloading, return `201` with `"state":"provisioning"` and fire
  `room.ready` webhook when done. Target: `ready` ≤ 10 s for a cached ROM, ≤ 60 s cold.
- Errors: `400` (validation, machine-readable `code` field), `401`, `413` (rom too
  big), `503 {"code":"no_capacity","retry_after_sec":15}`.
- Internal worker room ids stay server-generated (`<room>-p<N>___<game>` scheme
  exists); the API never exposes or accepts internal ids.

#### FR-1.2 `GET /v1/rooms/{room_id}`
State + per-player `{player, ref, connected, connected_at, last_seen, last_save_at}` +
`created_at/started_at`. `404` after `closed` + 10 min retention.

#### FR-1.3 `DELETE /v1/rooms/{room_id}`
Graceful close: freeze input → final SRAM flush → upload all saves → teardown →
`room.closed` webhook. Returns `202` immediately.

#### FR-1.4 `POST /v1/rooms/{room_id}/players/{n}/token`
Re-mints a token for that seat (page refresh after expiry). Returns same shape as the
`players[]` element. Old token stays valid until `exp` but the seat rule (FR-2.3)
means only one live connection exists.

#### FR-1.5 `GET /v1/capacity`
`{"free_rooms": n, "total_rooms": m, "by_players": {"2": n2, "4": n4}}` for this
container. Rebit uses it to gate "Start" and to fail over between regions.

#### FR-1.6 `GET /healthz`
Unauthenticated liveness (container health checks). No other unauthenticated route.

### FR-2 Player session tokens

- **FR-2.1** JWT HS256, secret from env (`NDS_TOKEN_SECRET`, never shared with Rebit
  clients). Claims: `rid` (room), `p` (player 1..4), `ref`, `exp` (now + 10 min),
  `jti`.
- **FR-2.2** Signaling WS (`/ws`) accepts **only** `?token=` for NDS rooms; the legacy
  `?room_id=&zone=` join path is rejected for rooms created via `/v1`. Token is
  validated at WS upgrade; failure → HTTP 401 close.
- **FR-2.3** One live connection per seat: a newer valid connection for the same
  `(rid, p)` terminates the older one (handles refresh/reconnect deterministically).
- **FR-2.4** Token expiry gates *connect only*; established WebRTC sessions live until
  room close.

### FR-3 Direct streaming protocol (consumed by the Rebit SDK)

This section is the **wire contract** for `@rebit/nds-stream`. Normative reference
implementations: `web/js/network/socket.js`, `web/js/network/webrtc.js`,
`web/js/api.js`, `web/js/input/{retropad,touch,pointer,keys}.js`. The service keeps
these semantics; simplifications below are service-side changes for NDS rooms.

**FR-3.1 Signaling transport.** WSS to `signaling_url`. Messages are JSON
`{"t": <int>, "id": <string, optional>, "p": <payload, optional>}`.

**FR-3.2 Session flow (NDS mode — server-driven, no game browsing):**

| Step | Direction | `t` | Payload |
|---|---|---|---|
| 1 | S→C | 4 `INIT` | `{ice: [IceServer], wid, games}` — for NDS rooms `ice` MUST include the room's TURN credentials; `games` is empty |
| 2 | C→S | 100 `INIT_WEBRTC_STREAM` | `{initiator: true, sdp: <offer JSON-string>}` — client is the offerer; recvonly video+audio transceivers; data channel `"data"` created **negotiated, id 0, unordered, maxRetransmits 0** |
| 3 | S→C | 100 | `{sdp: <answer>}` (plus 101 `WEBRTC_SIGNAL` `{ice}` trickle both ways; client may also send complete-gathered SDP) |
| 4 | C→S | 104 `GAME_START` | `{}` — **NDS change:** server already knows game/room/seat from the token; server MUST ignore client-supplied `game_name/room_id/player_index` |
| 5 | S→C | 104 | `{roomId, av: {video: {w,h,a,s}, audio:{hz}}, kb_mouse, pointer: true}` |
| 6 | — | — | media flows; server opens data channels `"keyboard"` and `"pointer"` (`pointer: true` always for melonDS) |

Errors: 112 `ERROR_NO_FREE_SLOTS` (seat taken by a newer connection), WS close codes
`4001` bad token, `4004` room not found/closed.

**FR-3.3 Input wire formats** (byte-exact; DataView values are **big-endian**, the
retropad array is **little-endian** — matches existing worker parsing):

- Channel `"data"` — RetroPad state, sent only on change, ≤ 60 Hz. 10 bytes,
  `Int16Array(5)` LE: `[buttons_bitmask, lx, ly, rx, ry]`. Bit order (libretro):
  `B,Y,SELECT,START,UP,DOWN,LEFT,RIGHT,A,X,L,R,L2,R2,L3,R3` (bit 0 = B). Axes unused
  for NDS → 0.
- Channel `"pointer"` — DS touch. 5 bytes: `u8 pressed(0|1), i16 x, i16 y` (BE).
  Coordinates in libretro pointer space: `round(unit * 65535 - 32768)` where `unit` is
  the touch position over the **full composited frame** (both DS screens stacked,
  256×384; the touch screen is the bottom half). SDK must send `pressed=0` with last
  coords on release.
- Channel `"keyboard"` — 7 bytes: `u32 keycode, u8 pressed, u16 mods` (BE). Optional
  for NDS; SDK may omit UI for it.

**FR-3.4 Media.** Video H264 (baseline, keyframe interval per existing config) or
VP8; Opus stereo audio (SDP munge `stereo=1` on `fmtp:111` required — keep the
existing workaround). Native frame 256×384 (dual screen stacked); client crops/lays
out the two 256×192 halves itself (CSS transforms on the `<video>`), which lets Rebit
render side-by-side or focus modes without server changes.

**FR-3.5 Reconnect.** On WS/PC drop the SDK retries: reuse token if `exp` valid, else
ask Rebit backend for `POST …/players/{n}/token` (FR-1.4) and restart from step 1.
Server keeps the seat playing (emulator never pauses; DS wireless continues).

### FR-4 ROM & save lifecycle

- **FR-4.1 ROM:** download to `nds/<sha1>.nds` (content-addressed → cross-room cache
  on the container), verify SHA1, reject mismatch (`room.failed`,
  `reason:"rom_hash_mismatch"`). Keep 512 MB cap + zip support. LRU-evict the cache at
  `NDS_ROM_CACHE_MAX_BYTES` (default 20 GB).
- **FR-4.2 Save in:** per seat, presigned GET (raw `.srm` or zip) — existing
  `installNDSSave` path, staged before the emulator boots.
- **FR-4.3 Save out (new, critical):**
  1. Flush melonDS SRAM to disk every **60 s if dirty** (hash-compare) and on: player
     disconnect, room close, SIGTERM.
  2. After each dirty flush: HTTP PUT raw bytes to `save_upload_url`
     (`Content-Type: application/octet-stream`), 3 retries w/ backoff. Never zip the
     uploaded body.
  3. Emit `save.uploaded` webhook `{player, ref, sha1, size, flushed_at}`.
  4. `room.closed` includes per-seat `save_status: "uploaded"|"unchanged"|"failed"`.
- **FR-4.4** The service persists nothing after teardown: wipe the room's runtime dir
  on recycle; CloudRetro's built-in S3 save store stays **disabled** for NDS rooms
  (Rebit is the single source of truth).

### FR-5 Room lifecycle & webhooks

**FR-5.1 States:** `provisioning → ready → active → closing → closed | failed`.
`active` on first player WebRTC-connected. Timers: `join_timeout` (from `ready`),
`idle_timeout` (all seats disconnected), `max_duration` (from `active`). All expiries
go through the graceful-close path (final save upload). This **replaces** the current
fixed 10-min reservation TTL.

**FR-5.2 Worker groups are recycled after every room** (kill workers, wipe dirs,
respawn warm) — no melonDS state may leak between sessions.

**FR-5.3 Webhooks** (coordinator → `NDS_WEBHOOK_URL`, per-container env; Rebit's
endpoint). POST JSON; headers `X-NDS-Event`, `X-NDS-Delivery` (uuid),
`X-NDS-Timestamp`, `X-NDS-Signature: sha256=HMAC_SHA256(secret, timestamp + "." + body)`.
At-least-once, retries 5× (1 s→2 min backoff). Events:

| Event | Extra payload | Fired when |
|---|---|---|
| `room.ready` | — | ROM installed + saves staged + workers up |
| `room.failed` | `reason` | provisioning error |
| `player.connected` / `player.disconnected` | `player, ref` | WebRTC session up/down |
| `room.started` | — | first player connected |
| `save.uploaded` | `player, ref, sha1, size, flushed_at` | each save PUT ok |
| `room.closed` | `reason: host_close\|idle\|max_duration\|join_timeout\|error`, `players[].save_status`, `duration_sec` | teardown complete |

### FR-6 Security requirements (service)

- **SR-1** API-key middleware on all `/v1`; no CORS on control plane; delete legacy
  open endpoint.
- **SR-2** Download guard in `pkg/worker/ndsapi.go`: allowlist hosts via
  `NDS_DOWNLOAD_ALLOWED_HOSTS` (e.g. `*.b-cdn.net,cdn.rebitplay.com`); resolve and
  reject non-public IPs (RFC1918/loopback/link-local/ULA) **including after
  redirects** (`http.Client.CheckRedirect`); keep size caps. Same allowlist applies to
  save PUT targets.
- **SR-3** ROM SHA1 verification (FR-4.1).
- **SR-4** JWT seat tokens (FR-2); reject unauthenticated joins for NDS rooms.
- **SR-5** Webhook HMAC + timestamp (±5 min window) to prevent replay/forgery.
- **SR-6** Rate limits: 30 room-creates/min per API key; 10 WS connects/min per IP.
- **SR-7** Workers run as non-root; per-room runtime dirs `0700`, wiped on recycle.
- **SR-8** TURN credentials minted per session (HMAC time-limited style,
  `username = expiry:room:player`) and delivered only via the API response / INIT
  message — no static TURN secret in any client bundle.

### FR-7 Deployment on Bunny Magic Containers

- **FR-7.1** One image, current `bunny-entrypoint.sh` model (coordinator + Xvfb +
  spawner + mux). New env: `NDS_API_KEY(S)`, `NDS_TOKEN_SECRET`, `NDS_WEBHOOK_URL`,
  `NDS_WEBHOOK_SECRET`, `NDS_PUBLIC_ENDPOINT`, `NDS_DOWNLOAD_ALLOWED_HOSTS`,
  `NDS_TURN_*`, `NDS_ROM_CACHE_MAX_BYTES`.
- **FR-7.2 Room affinity:** all URLs in API responses (signaling, endpoint) MUST be
  built from `NDS_PUBLIC_ENDPOINT` (this container's unique public hostname), **not**
  from the request Host — a room's players must all reach the exact container that
  hosts it. Anycast + the WebRTC mux (`BUNNY_ANYCAST_IP` / ufrag routing) remains the
  media fast path; TURN is the guaranteed fallback.
- **FR-7.3** Capacity: rooms spawn on demand up to `NDS_MAX_ROOM_COUNT` (existing
  spawner), with `WARM_GROUPS` pre-spawned (default 1) so `ready` is fast. Sizing rule
  of thumb: ~1.5 vCPU per seat (emulator + encoder) — set `NDS_MAX_ROOM_COUNT` from
  the container's vCPU count; validate with the load test (§12).
- **FR-7.4** Graceful drain on SIGTERM: stop accepting creates, close active rooms via
  the graceful path (saves upload!), then exit ≤ 60 s. Required for Bunny redeploys.
- **FR-7.5** Rebit config maps region → container endpoint(s). Rebit picks region at
  create time (host's geo / lobby majority) and fails over on `503 no_capacity`.

## 6. Functional requirements — Rebit (`../rebit`)

- **FR-R1 Config** `config/services.php` → `nds_cloud`: `endpoints` (region ⇒ URL),
  `api_key`, `webhook_secret`.
- **FR-R2 Actions** (`app/Actions/NdsCloud/`, existing AsAction pattern):
  `CreateNdsCloudSession` (presign ROM GET from media library — SHA1 already stored;
  presign save GET from latest `GameSave` + save PUT per player; call `POST /v1/rooms`;
  persist), `CloseNdsCloudSession`, `RefreshPlayerToken`.
- **FR-R3 Persistence** migration `nds_cloud_sessions`: `id, room_id, game_id,
  host_user_id, region, state, players (json: user_id, player, ref, connected),
  created_at, started_at, closed_at, close_reason`.
- **FR-R4 Webhook endpoint** `POST /api/nds-cloud/webhook`: verify HMAC + timestamp,
  dedupe on `X-NDS-Delivery`, update session row, on `save.uploaded` register the new
  save bytes (already in Bunny at the presigned path) as a `GameSave` version, on
  `room.closed` finalize the session + surface `save_status:"failed"` to affected
  users, broadcast state changes to the lobby channel (existing rebit-signal /
  websocket infra).
- **FR-R5 Lobby flow** reuse existing netplay room UI: host opens NDS game →
  "Multiplayer (cloud)" → members join lobby → host Start → backend
  `CreateNdsCloudSession` → each member receives `{signaling_url, token, ice_servers}`
  over the lobby channel → SDK connects. Playtime via existing
  `GameSession`/heartbeats while the stream is connected.
- **FR-R6 SDK** `@rebit/nds-stream` (TS package, `resources/js/lib/nds-stream/` or
  workspace package):
  - `connect({signalingUrl, iceServers, videoEl, onState, onError})` implementing
    FR-3 exactly; exposes `sendPad(buttonsBitmask)`, `sendTouch(x01, y01, pressed)`
    (unit coords → pointer space per FR-3.3), `disconnect()`, `stats()` (RTT, fps,
    bitrate from `RTCPeerConnection.getStats`).
  - No UI. Rebit's existing `GameConsole` touch overlay / d-pad components drive it,
    so cloud NDS looks identical to local play.
  - Reconnect logic per FR-3.5 (token refresh via Rebit backend).
- **FR-R7 UX states** in the console UI: `provisioning` (spinner ≤ 60 s), `ready/
  waiting for players`, `connected`, `reconnecting`, `closed` (+ save status toast),
  `no_capacity` (host-facing retry).

## 7. Non-functional requirements

- **NR-1 Latency:** glass-to-glass ≤ 120 ms same-region p50, ≤ 200 ms p95; input-to-
  photon ≤ 100 ms p50. (Existing POC meets this on Bunny SG; keep as regression bar.)
- **NR-2 Join time:** Start-click → first frame ≤ 10 s p95 with cached ROM.
- **NR-3 Save durability:** a save newer than 60 s old is never lost on any graceful
  path (close/idle/drain); on container crash, loss window ≤ 60 s (last flush).
- **NR-4 Capacity:** 1 container (16 vCPU) sustains ≥ 2 concurrent 4-player rooms at
  NR-1 latency; verified by load test.
- **NR-5 Observability:** Prometheus metrics — rooms by state, seats connected, create
  latency, token failures, webhook retry count, save upload failures, per-worker
  fps/encode-time, `netpacketStats` (tx/rx/dropped). One structured audit log line per
  room create/close (room, refs, duration, reason, bytes streamed).
- **NR-6 Availability:** room state is in-container; a container restart kills its
  rooms (acceptable v1) but MUST emit `room.closed` `reason:"error"` on next boot for
  rooms found orphaned (persist a tiny room journal to disk).

## 8. Milestones (agent-ready work breakdown)

**M1 — Service hardening (`cloud-game`)** *(no Rebit dependency; do first)*
1. `/v1` API: auth middleware, create/get/delete/token/capacity endpoints, idempotent
   create, machine-readable errors. Refactor `pkg/coordinator/ndsapi.go`; delete
   legacy route + CORS. Types in `pkg/api/nds.go`.
2. JWT seat tokens + WS-upgrade validation + seat takeover (coordinator `hub.go`,
   `userhandlers.go`); NDS rooms reject legacy join.
3. NDS signaling simplification: server-driven `GAME_START` (ignore client fields,
   derive from token), TURN creds in `INIT`.
4. SSRF guard + SHA1 verify + content-addressed ROM cache w/ LRU
   (`pkg/worker/ndsapi.go`).
5. Save flush/upload loop + PUT + statuses (worker: hook the existing periodic-save
   machinery in `pkg/worker/caged/libretro`, reroute from S3 store to HTTP PUT).
6. Lifecycle state machine + timers + graceful close/drain + webhook sender
   (HMAC, retries, journal for NR-6).
7. Container env plumbing (FR-7.1/7.2), entrypoint updates, `GET /healthz`.

**M2 — Rebit SDK + protocol proof**
8. `@rebit/nds-stream` per FR-R6 against a real M1 container (port logic from
   `web/js/network/*.js` + `web/js/input/*.js`; do **not** import CloudRetro JS).
9. Protocol conformance test page (dev-only route) driving 2 seats headlessly.

**M3 — Rebit integration**
10. Config/actions/migration/webhook controller (FR-R1..R4).
11. Lobby wiring + console UI states + touch overlay binding (FR-R5, R7).
12. Save-version registration + failure surfacing.

**M4 — Production readiness**
13. Metrics + audit logs (NR-5); load test harness (headless SDK clients) proving
    NR-1..NR-4; region failover in Rebit (FR-7.5); runbook
    (`docs/runbook-nds.md`).

## 9. Acceptance criteria & test plan

**API (Go httptest, `pkg/coordinator`):**
- [ ] No auth → 401 on every `/v1` route; `healthz` open.
- [ ] Create with 2/3/4 players → 201 + N tokens; replay same `room` → 200, same
      session, fresh tokens; 5th player / bad sha1 / bad host URL → 400 with `code`.
- [ ] Capacity exhausted → 503 `no_capacity` + `retry_after_sec`.
- [ ] URLs in response built from `NDS_PUBLIC_ENDPOINT` regardless of request Host.

**Tokens/signaling:**
- [ ] Expired/garbage token → WS 401/4001; valid token connects; second connection for
      same seat kills the first (old client gets 112); token for room A can't join
      room B.
- [ ] `GAME_START` with forged `player_index`/`room_id` is ignored (seat from token).

**SSRF guard (table tests):** private IPs, redirect-to-private, disallowed host,
oversize Content-Length, sha1 mismatch — all rejected; allowlisted Bunny host passes.

**Save lifecycle (fake PUT server):**
- [ ] Dirty flush uploads within 60 s; unchanged SRAM doesn't re-upload.
- [ ] Disconnect, DELETE room, idle expiry, SIGTERM drain: each produces a final
      upload + correct `save_status` in `room.closed`.
- [ ] PUT failing 4× → `save_status:"failed"` and webhook still delivered.

**End-to-end (extends `pkg/worker/caged/libretro/netpacket_room_test.go` + headless
SDK):** 4-seat room boots, all seats stream ≥ 30 s, netpacket peers see each other,
touch input on seat 2 reaches only seat 2's core, saves round-trip, room recycles
clean (no files left, group reusable).

**Rebit (Pest):** webhook HMAC/timestamp/dedupe; `CreateNdsCloudSession` presigns
correctly and stores session; `save.uploaded` creates a `GameSave` version;
lobby-channel broadcasts on state changes.

**Load (M4):** 16 vCPU container, 2×4-player rooms, 30 min: p95 latency within NR-1,
zero save-upload failures, no worker OOM/crash.

## 10. Open questions (owner: product/bang)

1. **Codec default per region** — H264 hardware encode availability on Bunny Magic
   Containers CPUs is unknown; if software-only, benchmark H264 vs VP8 at 256×384 and
   pick the cheaper default (options field already covers per-room override).
2. **Entitlements** — is cloud multiplayer gated by subscription tier? (Enforced in
   Rebit at create time; service only rate-limits per key.)
3. **Player-1 exit behavior** — melonDS netpacket host is seat 1 (commit `190646af`).
   Empirically test mid-game host quit; v1 policy: room stays up, seat 1 rejoin
   allowed while room `active`; if the game itself breaks, users end the session
   (document in runbook). Revisit if support load appears.
4. **Region set at launch** — start SG + one EU/US? Drives Rebit config only.
