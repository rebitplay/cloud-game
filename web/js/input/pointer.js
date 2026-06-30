import {
    pub,
    MOUSE_PRESSED,
    MOUSE_MOVED,
    POINTER_UPDATED,
} from "event";

const hasRawPointer = "onpointerrawupdate" in window;
const moveEvent = hasRawPointer ? "pointerrawupdate" : "pointermove";

// Reusable event data objects to avoid allocations
const move = { dx: 0, dy: 0 };
const btn = { b: null, p: false };

// Game resolution for DPI scaling
const gameW = 640;
const gameH = 480;

// Accumulates fractional pixels to prevent drift when scaling
let errX = 0;
let errY = 0;

const scaleDpi = (dx, dy, srcW, srcH) => {
    move.dx = dx / (srcW / gameW) + errX;
    move.dy = dy / (srcH / gameH) + errY;
    errX = move.dx % 1;
    errY = move.dy % 1;
    move.dx = Math.trunc(move.dx);
    move.dy = Math.trunc(move.dy);
    return move;
};

const onDown = (e) => {
    btn.b = e.button;
    btn.p = true;
    pub(MOUSE_PRESSED, btn);
};
const onUp = (e) => {
    btn.b = e.button;
    btn.p = false;
    pub(MOUSE_PRESSED, btn);
};

/*
 * Tracks pointer movement and publishes events.
 * Uses raw pointer events when available for better accuracy.
 * Coalesced events are broken in Firefox 120+, so we skip them there.
 */
const track = (el, getDisplay) => {
    let off = null;

    const handle = (e) => {
        const events = e.getCoalescedEvents?.() || [e];
        const { w, h, s } = getDisplay();

        for (const ev of events) {
            move.dx = ev.movementX;
            move.dy = ev.movementY;
            pub(MOUSE_MOVED, s ? scaleDpi(move.dx, move.dy, w, h) : move);
        }
    };

    return (on) => {
        if (on && !off) {
            el.addEventListener(moveEvent, handle);
            el.onpointerdown = onDown;
            el.onpointerup = onUp;
            off = () => {
                el.removeEventListener(moveEvent, handle);
                el.onpointerdown = null;
                el.onpointerup = null;
            };
        } else if (!on && off) {
            off();
            off = null;
        }
    };
};

/*
 * Auto-hides cursor after inactivity. Movement shows it again.
 */
const autoHide = (el, timeout = 3000) => {
    let timer;
    const cl = el.classList;
    const reset = () => {
        cl.remove("no-pointer");
        clearTimeout(timer);
        timer = setTimeout(() => cl.add("no-pointer"), timeout);
    };

    return (on) => {
        clearTimeout(timer);
        el.removeEventListener("pointermove", reset);
        cl.remove("no-pointer");
        if (on) {
            el.addEventListener("pointermove", reset);
            reset();
        }
    };
};

const pointerCoord = (value, size) => {
    if (!size) return 0;
    const unit = Math.max(0, Math.min(1, value / size));
    return Math.round(unit * 65535 - 32768);
};

const contentRect = (el, getContentSize) => {
    const r = el.getBoundingClientRect();
    const size = getContentSize?.();
    if (!size?.w || !size?.h) return r;

    const contentAspect = size.w / size.h;
    const boxAspect = r.width / r.height;
    if (!contentAspect || !boxAspect) return r;

    if (contentAspect > boxAspect) {
        const h = r.width / contentAspect;
        return {
            left: r.left,
            top: r.top + (r.height - h) / 2,
            width: r.width,
            height: h,
        };
    }

    const w = r.height * contentAspect;
    return {
        left: r.left + (r.width - w) / 2,
        top: r.top,
        width: w,
        height: r.height,
    };
};

const trackTouch = (el, getContentSize) => {
    let off = null;
    let active = null;

    const publish = (e, pressed) => {
        const r = contentRect(el, getContentSize);
        pub(POINTER_UPDATED, {
            pressed,
            x: pointerCoord(e.clientX - r.left, r.width),
            y: pointerCoord(e.clientY - r.top, r.height),
        });
    };

    const down = (e) => {
        active = e.pointerId;
        el.setPointerCapture(active);
        publish(e, true);
    };
    const move = (e) => {
        if (e.pointerId !== active) return;
        publish(e, true);
    };
    const up = (e) => {
        if (e.pointerId !== active) return;
        publish(e, false);
        el.releasePointerCapture(active);
        active = null;
    };

    return (on) => {
        if (on && !off) {
            el.style.touchAction = "none";
            el.addEventListener("pointerdown", down);
            el.addEventListener("pointermove", move);
            el.addEventListener("pointerup", up);
            el.addEventListener("pointercancel", up);
            off = () => {
                el.removeEventListener("pointerdown", down);
                el.removeEventListener("pointermove", move);
                el.removeEventListener("pointerup", up);
                el.removeEventListener("pointercancel", up);
                el.style.touchAction = "";
            };
        } else if (!on && off) {
            off();
            off = null;
            active = null;
            pub(POINTER_UPDATED, { pressed: false, x: 0, y: 0 });
        }
    };
};

export const pointer = {
    lock: (el) => el.requestPointerLock(),
    track,
    trackTouch,
    autoHide,
};
