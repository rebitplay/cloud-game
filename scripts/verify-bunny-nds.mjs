#!/usr/bin/env node

const endpoint = normalizeEndpoint(requiredEnv('NDS_ENDPOINT'));
const endpointURL = new URL(endpoint);
const apiKey = stringEnv('NDS_API_KEY', '');
const publicPreflightOnly = boolEnv('NDS_PUBLIC_PREFLIGHT_ONLY', false);
const expectedVersion = stringEnv('NDS_EXPECT_VERSION', '');
const expectedAssetMin = intEnv('NDS_EXPECT_WEBRTC_ASSET_MIN', 10);
const expectedPublicIP = stringEnv(
    'NDS_EXPECT_PUBLIC_IP',
    /^\d+\.\d+\.\d+\.\d+$/.test(endpointURL.hostname) ? endpointURL.hostname : '',
);
const expectedPublicPort = stringEnv('NDS_EXPECT_PUBLIC_PORT', '8641');
const requireTurn = boolEnv('NDS_REQUIRE_TURN', true);
const requireMetrics = boolEnv('NDS_REQUIRE_METRICS', true);
const rooms = intEnv('NDS_ROOMS', 2);
const players = intEnv('NDS_PLAYERS', 4);

const report = {
    endpoint,
    expected: {
        version: expectedVersion || null,
        webrtc_asset_min: expectedAssetMin,
        public_ip: expectedPublicIP || null,
        public_port: expectedPublicPort || null,
        require_turn: requireTurn,
        require_metrics: requireMetrics,
        public_preflight_only: publicPreflightOnly,
        rooms,
        players,
    },
    probes: {},
    checks: [],
};

const health = await probe('healthz', () => request('/healthz', { expected: [204] }));
check('healthz_204', health.ok && health.status === 204, statusDetail(health));

const publicBuildz = await probe('buildz_public_route', () => request('/buildz', { expected: [200, 401] }));
check(
    'buildz_route_exists',
    publicBuildz.ok,
    publicBuildz.ok
        ? `HTTP ${publicBuildz.status}; route exists${publicBuildz.status === 401 ? ' and requires auth' : ''}`
        : `${statusDetail(publicBuildz)}; latest image should return 200 or 401, not 404`,
);

if (!publicPreflightOnly) {
    check(
        'api_key_present',
        apiKey !== '',
        apiKey ? 'present' : 'NDS_API_KEY is required for /buildz, /v1/capacity, and /metrics',
    );

    const buildz = await probe('buildz', () => request('/buildz', { auth: true, expected: [200] }));
    if (buildz.ok) {
        const build = buildz.json ?? {};
        check(
            'buildz_version',
            !expectedVersion || build.version === expectedVersion,
            expectedVersion ? `got ${jsonValue(build.version)}, want ${expectedVersion}` : `got ${jsonValue(build.version)}; no exact version required`,
        );
        check(
            'buildz_webrtc_asset_min',
            webRTCAssetVersion(build.webrtc_asset) >= expectedAssetMin,
            `got ${jsonValue(build.webrtc_asset)}, want >= v${expectedAssetMin}`,
        );
        check(
            'buildz_firefox_relay_policy',
            build.webrtc_firefox_relay_policy === true,
            `got ${jsonValue(build.webrtc_firefox_relay_policy)}`,
        );
        check('buildz_mux_enabled', build.webrtc_mux_enabled === true, `got ${jsonValue(build.webrtc_mux_enabled)}`);
        check(
            'buildz_public_ip',
            !expectedPublicIP || build.webrtc_public_ip === expectedPublicIP,
            `got ${jsonValue(build.webrtc_public_ip)}, want ${expectedPublicIP}`,
        );
        check(
            'buildz_public_port',
            !expectedPublicPort || String(build.webrtc_public_port) === expectedPublicPort,
            `got ${jsonValue(build.webrtc_public_port)}, want ${expectedPublicPort}`,
        );
        check(
            'buildz_turn_configured',
            !requireTurn || build.nds_rest_turn_configured === true || Number(build.config_turn_count ?? 0) > 0,
            requireTurn
                ? `nds_rest_turn_configured=${jsonValue(build.nds_rest_turn_configured)}, config_turn_count=${jsonValue(build.config_turn_count)}`
                : 'TURN not required for this verifier run',
        );
        check('buildz_metrics_enabled', !requireMetrics || build.metrics_enabled === true, `got ${jsonValue(build.metrics_enabled)}`);
    } else {
        check('buildz_available', false, statusDetail(buildz));
    }
} else {
    check('public_preflight_mode', true, 'skipping authenticated /buildz, /v1/capacity, and /metrics checks');
}

if (!publicPreflightOnly) {
    const capacity = await probe('capacity', () => request('/v1/capacity', { auth: true, expected: [200] }));
    if (capacity.ok) {
        const cap = capacity.json ?? {};
        const playerCapacity = Number(cap.by_players?.[String(players)] ?? 0);
        check('capacity_total_rooms', Number(cap.total_rooms ?? 0) >= rooms, `got ${jsonValue(cap.total_rooms)}, want >= ${rooms}`);
        check(
            'capacity_by_players',
            playerCapacity >= rooms,
            `by_players[${players}]=${playerCapacity}, want >= ${rooms}`,
        );
    } else {
        check('capacity_available', false, statusDetail(capacity));
    }
}

const demo = await probe('mkds_demo', () => request('/mkds-lan.html', { expected: [200] }));
if (demo.ok) {
    const body = demo.text ?? '';
    check(
        'demo_uses_v1_rooms',
        body.includes('/v1/rooms'),
        body.includes('/v1/rooms') ? 'found /v1/rooms' : 'mkds-lan.html should post to /v1/rooms',
    );
    check(
        'demo_not_old_api',
        !body.includes('/api/nds/rooms'),
        body.includes('/api/nds/rooms') ? 'mkds-lan.html still references /api/nds/rooms' : 'old API path absent',
    );
} else {
    check('demo_available', false, statusDetail(demo));
}

const networkJS = await probe('network_js', () => request('/js/network/network.js', { expected: [200] }));
if (networkJS.ok) {
    const asset = networkJS.text?.match(/webrtc\.js\?v=\d+/)?.[0] ?? '';
    check('network_js_webrtc_asset_min', webRTCAssetVersion(asset) >= expectedAssetMin, `got ${asset || 'missing'}`);
} else {
    check('network_js_available', false, statusDetail(networkJS));
}

const webrtcJS = await probe('webrtc_js', () => request('/js/network/webrtc.js', { expected: [200] }));
if (webrtcJS.ok) {
    const body = webrtcJS.text ?? '';
    check(
        'webrtc_js_firefox_relay_policy',
        body.includes('iceTransportPolicy = "relay"') && body.includes('Firefox detected'),
        body.includes('iceTransportPolicy = "relay"') && body.includes('Firefox detected')
            ? 'Firefox relay policy present'
            : 'Firefox relay policy code is missing',
    );
} else {
    check('webrtc_js_available', false, statusDetail(webrtcJS));
}

if (!publicPreflightOnly && requireMetrics) {
    const metrics = await probe('metrics', () => request('/metrics', { auth: true, expected: [200] }));
    if (metrics.ok) {
        check(
            'metrics_nds_rooms_present',
            (metrics.text ?? '').includes('cloud_game_nds_rooms'),
            (metrics.text ?? '').includes('cloud_game_nds_rooms') ? 'cloud_game_nds_rooms present' : 'cloud_game_nds_rooms missing',
        );
    } else {
        check('metrics_available', false, statusDetail(metrics));
    }
}

report.ok = report.checks.every((item) => item.ok);
console.log(JSON.stringify(report, null, 2));
if (!report.ok) {
    process.exitCode = 1;
}

async function probe(name, fn) {
    try {
        const result = await fn();
        report.probes[name] = compactProbe(result);
        return result;
    } catch (error) {
        const result = {
            ok: false,
            status: error.status ?? null,
            error: error.message,
            text: error.text,
            json: error.json,
        };
        report.probes[name] = compactProbe(result);
        return result;
    }
}

async function request(path, { auth = false, expected = [200] } = {}) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 10000);
    const headers = {};
    if (auth && apiKey) {
        headers.Authorization = `Bearer ${apiKey}`;
    }

    let response;
    try {
        response = await fetch(new URL(path, `${endpoint}/`), {
            headers,
            signal: controller.signal,
        });
    } finally {
        clearTimeout(timeout);
    }

    const text = await response.text();
    const json = parseJSON(text);
    if (!expected.includes(response.status)) {
        const error = new Error(`HTTP ${response.status}`);
        error.status = response.status;
        error.text = text;
        error.json = json;
        throw error;
    }
    return {
        ok: true,
        status: response.status,
        text,
        json,
    };
}

function compactProbe(result) {
    const out = {
        ok: result.ok === true,
        status: result.status ?? null,
    };
    if (result.error) {
        out.error = result.error;
    }
    if (result.json && typeof result.json === 'object') {
        out.json = result.json;
    } else if (result.text) {
        out.text = result.text.slice(0, 500);
    }
    return out;
}

function check(name, ok, detail = '') {
    report.checks.push({
        name,
        ok: Boolean(ok),
        detail,
    });
}

function statusDetail(result) {
    if (result.ok) return `HTTP ${result.status}`;
    if (result.status) return `HTTP ${result.status}: ${trim(result.text || result.error || '')}`;
    return trim(result.error || 'request failed');
}

function normalizeEndpoint(value) {
    return value.replace(/\/+$/, '');
}

function webRTCAssetVersion(asset) {
    const match = String(asset ?? '').match(/webrtc\.js\?v=(\d+)/);
    return match ? Number(match[1]) : 0;
}

function parseJSON(text) {
    try {
        return text ? JSON.parse(text) : null;
    } catch {
        return null;
    }
}

function jsonValue(value) {
    return JSON.stringify(value);
}

function trim(value) {
    return String(value).replace(/\s+/g, ' ').trim().slice(0, 240);
}

function requiredEnv(name) {
    const value = process.env[name];
    if (value === undefined || value.trim() === '') {
        throw new Error(`${name} is required`);
    }
    return value.trim();
}

function stringEnv(name, fallback) {
    const value = process.env[name];
    return value === undefined || value.trim() === '' ? fallback : value.trim();
}

function intEnv(name, fallback) {
    const value = process.env[name];
    if (value === undefined || value.trim() === '') return fallback;
    const parsed = Number.parseInt(value, 10);
    if (!Number.isFinite(parsed)) {
        throw new Error(`${name} must be an integer`);
    }
    return parsed;
}

function boolEnv(name, fallback) {
    const value = process.env[name];
    if (value === undefined || value.trim() === '') return fallback;
    return !['0', 'false', 'no', 'off'].includes(value.trim().toLowerCase());
}
