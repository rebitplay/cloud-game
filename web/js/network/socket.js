import { pub, MESSAGE } from "event";
import { log } from "log";

let conn;

const buildUrl = (params = {}) => {
    const url = new URL(window.location);
    url.protocol = location.protocol !== "https:" ? "ws" : "wss";
    url.pathname = "/ws";
    Object.keys(params).forEach((k) => {
        if (params[k]) url.searchParams.set(k, params[k]);
    });
    return url;
};

const init = (roomId, wid, zone) => {
    let objParams = { room_id: roomId, zone: zone };
    if (wid) objParams.wid = wid;
    const url = buildUrl(objParams);
    log.debug(`[ws] connecting to ${url}`);
    conn = new WebSocket(url.toString());
    conn.onopen = () => log.debug("[ws] opened");
    conn.onerror = () => log.error("[ws] error");
    conn.onclose = (event) => log.debug(`[ws] closed (${event.code})`);
    conn.onmessage = (response) => pub(MESSAGE, JSON.parse(response.data));
};

const send = (data) => {
    if (conn?.readyState === WebSocket.OPEN) conn.send(JSON.stringify(data));
};

const close = () => {
    if (conn && conn.readyState < WebSocket.CLOSING) conn.close();
};

/**
 * WebSocket connection module.
 *
 *  Needs init() call.
 */
export const socket = {
    close,
    init,
    send,
};
