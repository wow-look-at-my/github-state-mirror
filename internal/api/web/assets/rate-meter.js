// <rate-meter> — one GitHub rate-limit bucket rendered as a self-contained web
// component (custom element + shadow DOM owning its markup and styles). Used by
// the admin "Rate limit" tab; self-registers on load, so index.html just adds
// <script type="module" src="assets/rate-meter.js"></script> and app.ts creates
// <rate-meter> elements with plain attributes:
//
//   <rate-meter name="core" limit="15000" remaining="14231" used="769"
//               reset="1767225600"></rate-meter>
//
// reset is Unix epoch seconds. The component reflects a computed `low`
// attribute on itself when remaining/limit < 15% (styled via :host([low]);
// `low` is deliberately NOT observed, so setting it can't recurse into
// attributeChangedCallback). While connected, a 1s interval keeps the
// "resets in …" countdown ticking between the dashboard's 5s refetches (and in
// the static preview, which never refetches); the tab rebuild replaces these
// elements every 5s, so disconnectedCallback must — and does — clear the timer.
//
// Layout notes (the fixes over the old light-DOM .rate-meter tile): the host is
// a flex column with min-width:0 so it can shrink inside the .rate-grid; the
// long mono resource name wraps (overflow-wrap:anywhere) instead of pushing the
// numbers out of the tile; the numbers never wrap (white-space:nowrap,
// flex-shrink:0); and the bar carries margin-top:auto so bars and footers align
// at the bottom of a stretched grid row.
const STYLE = `
:host {
    display: flex;
    flex-direction: column;
    box-sizing: border-box;
    min-width: 0;
    background: var(--bg);
    border: 1px solid var(--border-muted);
    border-radius: 8px;
    padding: 12px 14px;
}
:host([low]) { border-color: rgba(248, 81, 73, 0.5); }
.top {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 8px;
}
.name {
    font-family: var(--mono);
    font-size: 13px;
    font-weight: 600;
    min-width: 0;
    overflow-wrap: anywhere;
}
.nums {
    font-size: 12px;
    color: var(--fg-muted);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
    flex-shrink: 0;
}
.bar {
    height: 6px;
    border-radius: 999px;
    background: var(--border-muted);
    overflow: hidden;
    margin-top: auto;
}
.fill {
    height: 100%;
    background: var(--green);
    border-radius: 999px;
    transition: width 0.3s;
}
:host([low]) .fill { background: var(--red); }
.foot {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    margin-top: 6px;
    font-size: 11px;
    color: var(--fg-muted);
    font-variant-numeric: tabular-nums;
}
:host([low]) .reset { color: var(--yellow); }
`;
// fmtUntil renders the time remaining until a Unix-epoch reset, e.g. "in 42m"
// or "in 1h 3m"; "now" once the window has passed.
function fmtUntil(epochSeconds) {
    const secs = Math.round(Number(epochSeconds) - Date.now() / 1000);
    if (!isFinite(secs) || secs <= 0)
        return "now";
    const h = Math.floor(secs / 3600);
    const m = Math.floor((secs % 3600) / 60);
    const s = secs % 60;
    if (h > 0)
        return "in " + h + "h " + m + "m";
    if (m > 0)
        return "in " + m + "m " + s + "s";
    return "in " + s + "s";
}
class RateMeterElement extends HTMLElement {
    constructor() {
        super();
        const root = this.attachShadow({ mode: "open" });
        const style = document.createElement("style");
        style.textContent = STYLE;
        this.nameEl = document.createElement("span");
        this.nameEl.className = "name";
        this.numsEl = document.createElement("span");
        this.numsEl.className = "nums";
        const top = document.createElement("div");
        top.className = "top";
        top.append(this.nameEl, this.numsEl);
        this.fillEl = document.createElement("div");
        this.fillEl.className = "fill";
        const bar = document.createElement("div");
        bar.className = "bar";
        bar.append(this.fillEl);
        this.usedEl = document.createElement("span");
        this.resetEl = document.createElement("span");
        this.resetEl.className = "reset";
        const foot = document.createElement("div");
        foot.className = "foot";
        foot.append(this.usedEl, this.resetEl);
        root.append(style, top, bar, foot);
    }
    attrNum(name) {
        return Number(this.getAttribute(name)) || 0;
    }
    render() {
        const name = this.getAttribute("name") ?? "";
        const remaining = this.attrNum("remaining");
        const limit = this.attrNum("limit");
        const used = limit ? limit - remaining : this.attrNum("used");
        const pct = limit ? Math.max(0, Math.min(100, (remaining / limit) * 100)) : 100;
        const low = limit > 0 && remaining / limit < 0.15;
        this.nameEl.textContent = name;
        // The name wraps when narrow; a hover tooltip carries the full name.
        this.nameEl.title = name;
        this.numsEl.textContent = remaining + " / " + limit + " left";
        this.fillEl.style.width = pct.toFixed(1) + "%";
        this.usedEl.textContent = used + " used";
        this.tick();
        this.toggleAttribute("low", low);
    }
    // tick refreshes only the countdown text; the 1s interval keeps it live
    // between data refreshes.
    tick() {
        this.resetEl.textContent = "resets " + fmtUntil(this.attrNum("reset"));
    }
    connectedCallback() {
        this.render();
        this.timer = window.setInterval(() => this.tick(), 1000);
    }
    disconnectedCallback() {
        if (this.timer !== undefined) {
            window.clearInterval(this.timer);
            this.timer = undefined;
        }
    }
    attributeChangedCallback() {
        this.render();
    }
}
// `low` is computed and reflected by render(); observing it would make
// toggleAttribute("low") re-enter attributeChangedCallback.
RateMeterElement.observedAttributes = ["name", "limit", "remaining", "used", "reset"];
customElements.define("rate-meter", RateMeterElement);
export {};
