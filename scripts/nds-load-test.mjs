#!/usr/bin/env node

import { chromium, firefox, webkit } from 'playwright';

const endpoint = requiredEnv('NDS_ENDPOINT').replace(/\/+$/, '');
const apiKey = requiredEnv('NDS_API_KEY');
const romUrl = requiredEnv('NDS_ROM_URL');
const romSha1 = requiredEnv('NDS_ROM_SHA1').toLowerCase();
const romName = stringEnv('NDS_ROM_NAME', 'load-test.nds');
const saveUploadUrl = requiredEnv('NDS_SAVE_UPLOAD_URL');
const rooms = intEnv('NDS_ROOMS', 2);
const players = intEnv('NDS_PLAYERS', 4);
const durationSec = intEnv('NDS_DURATION_SEC', 1800);
const joinTimeoutMs = intEnv('NDS_JOIN_TIMEOUT_MS', 15000);
const joinP95MaxMs = intEnv('NDS_JOIN_P95_MAX_MS', 10000);
const rttP95MaxMs = intEnv('NDS_RTT_P95_MAX_MS', 200);
const headless = envFlag('NDS_HEADLESS', true);
const browserName = stringEnv('NDS_BROWSER', 'chromium').toLowerCase();
const browserTypes = { chromium, firefox, webkit };

if (!/^[a-f0-9]{40}$/.test(romSha1)) {
    throw new Error('NDS_ROM_SHA1 must be a 40-character hex SHA1');
}
if (players < 2 || players > 4) {
    throw new Error('NDS_PLAYERS must be 2, 3, or 4');
}
if (!browserTypes[browserName]) {
    throw new Error(`NDS_BROWSER must be one of: ${Object.keys(browserTypes).join(', ')}`);
}

let browser;

async function main() {
    browser = await browserTypes[browserName].launch({ headless });
    const startedAt = Date.now();
    const roomRuns = [];

    try {
        for (let i = 0; i < rooms; i += 1) {
            const roomId = `load-${Date.now().toString(36)}-${i}`;
            roomRuns.push(await startRoom(roomId));
        }

        await wait(durationSec * 1000);
    } finally {
        for (const room of roomRuns) {
            await room.close();
        }
        await browser.close();
    }

    const samples = roomRuns.flatMap((room) => room.clients.map((client) => client.summary()));
    const joinMs = samples.map((sample) => sample.joinMs).filter((value) => Number.isFinite(value));
    const rttMs = samples.flatMap((sample) => sample.rttMs);
    const fps = samples.flatMap((sample) => sample.fps);
    const checks = evaluateChecks({ samples, joinMs, rttMs });

    console.log(
        JSON.stringify(
            {
                durationSec: Math.round((Date.now() - startedAt) / 1000),
                browser: browserName,
                rooms,
                players,
                clients: samples.length,
                joinMs: summary(joinMs),
                rttMs: summary(rttMs),
                fps: summary(fps),
                checks,
                samples,
            },
            null,
            2,
        ),
    );

    if (!checks.ok) {
        process.exitCode = 1;
    }
}

async function startRoom(roomId) {
    const room = await createRoom(roomId);
    const context = await browser.newContext();
    const clients = [];

    for (const join of room.players) {
        const page = await context.newPage();
        const client = new NdsHeadlessClient(page, roomId, join.player);
        clients.push(client);
        await client.connect(join.signaling_url, join.ice_servers ?? []);
    }

    await Promise.all(clients.map((client) => client.waitForFirstFrame(joinTimeoutMs)));

    return {
        roomId,
        clients,
        close: async () => {
            await Promise.allSettled(clients.map((client) => client.close()));
            await context.close().catch(() => undefined);
            await fetch(`${endpoint}/v1/rooms/${encodeURIComponent(roomId)}`, {
                method: 'DELETE',
                headers: { Authorization: `Bearer ${apiKey}` },
            }).catch(() => undefined);
        },
    };
}

async function createRoom(roomId) {
    const payload = {
        room: roomId,
        players,
        rom: {
            name: romName,
            sha1: romSha1,
            url: romUrl,
        },
        player_slots: Array.from({ length: players }, (_, index) => {
            const player = index + 1;
            return {
                player,
                ref: `load_p${player}`,
                save_url: '',
                save_upload_url: saveUploadUrl.replaceAll('{room}', encodeURIComponent(roomId)).replaceAll('{player}', String(player)),
            };
        }),
        options: {
            idle_timeout_sec: durationSec + 120,
            join_timeout_sec: 120,
            max_duration_sec: durationSec + 300,
        },
    };

    const response = await fetch(`${endpoint}/v1/rooms`, {
        method: 'POST',
        headers: {
            Authorization: `Bearer ${apiKey}`,
            'Content-Type': 'application/json',
        },
        body: JSON.stringify(payload),
    });
    const body = await response.text();
    if (!response.ok) {
        throw new Error(`room create failed ${response.status}: ${body}`);
    }
    return JSON.parse(body);
}

class NdsHeadlessClient {
    constructor(page, roomId, player) {
        this.page = page;
        this.roomId = roomId;
        this.player = player;
        this.startedAt = Date.now();
        this.firstFrameAt = null;
        this.stats = [];
    }

    async connect(signalingUrl, iceServers) {
        await this.page.goto('about:blank');
        await this.page.exposeFunction(`recordStats_${this.player}`, (sample) => {
            this.stats.push(sample);
            if (sample.firstFrame && this.firstFrameAt === null) {
                this.firstFrameAt = Date.now();
            }
        });
        await this.page.evaluate(
            ({ signalingUrl, iceServers, player }) => {
                const NDS_PACKET = {
                    INIT: 4,
                    INIT_WEBRTC_STREAM: 100,
                    WEBRTC_SIGNAL: 101,
                    GAME_START: 104,
                };
                const stream = new MediaStream();
                const video = document.createElement('video');
                video.muted = true;
                video.autoplay = true;
                video.playsInline = true;
                video.srcObject = stream;
                document.body.append(video);

                const pc = new RTCPeerConnection({ iceServers });
                const ws = new WebSocket(signalingUrl);
                const data = pc.createDataChannel('data', {
                    negotiated: true,
                    id: 0,
                    ordered: false,
                    maxRetransmits: 0,
                });
                let offerSent = false;
                const localIce = [];
                const remoteIce = [];
                let firstFrame = false;

                pc.addTransceiver('video', { direction: 'recvonly' });
                pc.addTransceiver('audio', { direction: 'recvonly' });
                pc.ontrack = (event) => {
                    stream.addTrack(event.track);
                    video.play().catch(() => undefined);
                };
                pc.onicecandidate = (event) => {
                    if (!event.candidate) return;
                    const candidate = event.candidate.toJSON();
                    if (!offerSent) {
                        localIce.push(candidate);
                        return;
                    }
                    send(NDS_PACKET.WEBRTC_SIGNAL, { ice: JSON.stringify(candidate) });
                };
                pc.onconnectionstatechange = () => {
                    if (pc.connectionState === 'connected') {
                        send(NDS_PACKET.GAME_START, {});
                    }
                };

                const send = (t, p) => {
                    if (ws.readyState === WebSocket.OPEN) {
                        ws.send(JSON.stringify({ t, p }));
                    }
                };
                const flushRemoteIce = () => {
                    if (!pc.remoteDescription) return;
                    while (remoteIce.length > 0) {
                        const candidate = remoteIce.shift();
                        pc.addIceCandidate(candidate ? new RTCIceCandidate(candidate) : null).catch(() => undefined);
                    }
                };
                const addRemoteIce = (candidate) => {
                    if (!pc.remoteDescription) {
                        remoteIce.push(candidate);
                        return;
                    }
                    pc.addIceCandidate(candidate ? new RTCIceCandidate(candidate) : null).catch(() => remoteIce.push(candidate));
                };
                const startPeer = async () => {
                    const offer = await pc.createOffer();
                    offer.sdp = (offer.sdp || '').replace(/(a=fmtp:111 .*)/g, '$1;stereo=1');
                    await pc.setLocalDescription(offer);
                    send(NDS_PACKET.INIT_WEBRTC_STREAM, {
                        initiator: true,
                        sdp: JSON.stringify({ type: pc.localDescription.type, sdp: pc.localDescription.sdp }),
                    });
                    offerSent = true;
                    while (localIce.length > 0) {
                        send(NDS_PACKET.WEBRTC_SIGNAL, { ice: JSON.stringify(localIce.shift()) });
                    }
                };
                const sendPad = () => {
                    if (data.readyState !== 'open') return;
                    data.send(new Int16Array([0, 0, 0, 0, 0]).buffer);
                };

                ws.onmessage = async (event) => {
                    const packet = JSON.parse(event.data);
                    const payload = packet.p || {};
                    if (packet.t === NDS_PACKET.INIT) {
                        await startPeer();
                    }
                    if ((packet.t === NDS_PACKET.INIT_WEBRTC_STREAM || packet.t === NDS_PACKET.WEBRTC_SIGNAL) && payload.sdp) {
                        const sdp = typeof payload.sdp === 'string' ? JSON.parse(payload.sdp) : payload.sdp;
                        await pc.setRemoteDescription(new RTCSessionDescription(sdp));
                        flushRemoteIce();
                    }
                    if ((packet.t === NDS_PACKET.INIT_WEBRTC_STREAM || packet.t === NDS_PACKET.WEBRTC_SIGNAL) && 'ice' in payload) {
                        addRemoteIce(payload.ice ? JSON.parse(payload.ice) : null);
                    }
                };

                video.addEventListener('playing', () => {
                    firstFrame = true;
                    window[`recordStats_${player}`]({ firstFrame: true, at: Date.now() });
                });
                setInterval(sendPad, 250);
                setInterval(async () => {
                    const report = await pc.getStats();
                    const sample = { at: Date.now(), rttMs: null, fps: null, bytesReceived: null, packetsLost: null };
                    report.forEach((item) => {
                        if (item.type === 'candidate-pair' && item.state === 'succeeded' && typeof item.currentRoundTripTime === 'number') {
                            sample.rttMs = Math.round(item.currentRoundTripTime * 1000);
                        }
                        if (item.type === 'inbound-rtp' && item.kind === 'video') {
                            sample.fps = item.framesPerSecond ?? null;
                            sample.bytesReceived = item.bytesReceived ?? null;
                            sample.packetsLost = item.packetsLost ?? null;
                        }
                    });
                    if (firstFrame) window[`recordStats_${player}`](sample);
                }, 1000);
            },
            { signalingUrl, iceServers, player: this.player },
        );
    }

    async waitForFirstFrame(timeoutMs) {
        const deadline = Date.now() + timeoutMs;
        while (Date.now() < deadline) {
            if (this.firstFrameAt !== null) return;
            await wait(100);
        }
        throw new Error(`room ${this.roomId} player ${this.player} did not receive first frame within ${timeoutMs}ms`);
    }

    async close() {
        await this.page.close().catch(() => undefined);
    }

    summary() {
        return {
            room: this.roomId,
            player: this.player,
            joinMs: this.firstFrameAt === null ? null : this.firstFrameAt - this.startedAt,
            rttMs: this.stats.map((sample) => sample.rttMs).filter((value) => Number.isFinite(value)),
            fps: this.stats.map((sample) => sample.fps).filter((value) => Number.isFinite(value)),
            last: this.stats.at(-1) ?? null,
        };
    }
}

function summary(values) {
    if (!values.length) return { count: 0, p50: null, p95: null, max: null };
    const sorted = [...values].sort((a, b) => a - b);
    return {
        count: sorted.length,
        p50: percentile(sorted, 0.5),
        p95: percentile(sorted, 0.95),
        max: sorted.at(-1),
    };
}

function percentile(sorted, q) {
    const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil(sorted.length * q) - 1));
    return sorted[index];
}

function evaluateChecks({ samples, joinMs, rttMs }) {
    const expectedClients = rooms * players;
    const join = summary(joinMs);
    const rtt = summary(rttMs);
    const checks = [
        {
            name: 'all_clients_reported',
            ok: samples.length === expectedClients,
            expected: expectedClients,
            actual: samples.length,
        },
        {
            name: 'all_clients_first_frame',
            ok: joinMs.length === expectedClients,
            expected: expectedClients,
            actual: joinMs.length,
        },
        {
            name: 'join_p95_ms',
            ok: join.p95 !== null && join.p95 <= joinP95MaxMs,
            threshold: joinP95MaxMs,
            actual: join.p95,
        },
        {
            name: 'rtt_p95_ms',
            ok: rtt.p95 !== null && rtt.p95 <= rttP95MaxMs,
            threshold: rttP95MaxMs,
            actual: rtt.p95,
        },
    ];

    return {
        ok: checks.every((check) => check.ok),
        checks,
    };
}

function wait(ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
}

function requiredEnv(name) {
    const value = process.env[name];
    if (!value) throw new Error(`${name} is required`);
    return value;
}

function intEnv(name, fallback) {
    const value = process.env[name];
    if (!value) return fallback;
    const parsed = Number.parseInt(value, 10);
    if (!Number.isFinite(parsed)) throw new Error(`${name} must be an integer`);
    return parsed;
}

function envFlag(name, fallback) {
    const value = process.env[name];
    if (value === undefined) return fallback;
    return !['0', 'false', 'no'].includes(value.toLowerCase());
}

function stringEnv(name, fallback) {
    const value = process.env[name];
    return value === undefined || value.trim() === '' ? fallback : value.trim();
}

await main();
