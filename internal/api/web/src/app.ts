// Source of truth for the dashboard front-end. Compiled by `npm run build`
// (tsc) to ../assets/app.js, which is committed (a generated artifact) and
// embedded into the Go binary. Edit this .ts file, never the .js.
//
// Talks to two endpoints:
//   GET /api/me                   -> Me
//   GET /api/cache?scope=mine|all -> CacheResponse
//
// When window.__GSM_DEMO__ is defined (only the standalone styling preview ships
// a demo-data.js that sets it), the same rendering runs against canned data and
// a state switcher is shown. Production never defines it, so the real backend is
// used.

interface Me {
    authenticated: boolean;
    login_configured: boolean;
    login?: string;
    is_admin: boolean;
}

interface Counts {
    repos: number;
    pull_requests: number;
    orgs: number;
    users: number;
    commit_checks: number;
    pr_files: number;
    branch_comparisons: number;
}

interface KindFreshness {
    kind: string;
    states: Record<string, number>;
    last_fetched?: string;
}

interface RecentRefresh {
    kind: string;
    key: string;
    trigger: string;
    started_at: string;
    status: string;
}

interface ScopeStats {
    actor: string;
    login: string;
    is_self: boolean;
    last_seen?: string;
    counts: Counts;
    total: number;
    kinds: KindFreshness[] | null;
    recent?: RecentRefresh[];
}

interface CacheResponse {
    login: string;
    is_admin: boolean;
    scope: string;
    scope_count: number;
    totals: Counts;
    scopes: ScopeStats[] | null;
}

interface DemoStateData {
    me: Me;
    mine?: CacheResponse;
    all?: CacheResponse;
}

interface DemoConfig {
    initial: string;
    current?: string;
    data: Record<string, DemoStateData>;
}

declare global {
    interface Window {
        __GSM_DEMO__?: DemoConfig;
    }
}

const DEMO: DemoConfig | null = typeof window !== "undefined" ? window.__GSM_DEMO__ ?? null : null;

const COUNT_FIELDS: ReadonlyArray<readonly [keyof Counts, string]> = [
    ["repos", "Repos"],
    ["pull_requests", "Pull requests"],
    ["orgs", "Orgs"],
    ["users", "Users"],
    ["commit_checks", "Commit checks"],
    ["pr_files", "PR files"],
    ["branch_comparisons", "Comparisons"],
];

const STATE_ORDER: ReadonlyArray<string> = ["fresh", "stale", "fetching", "error", "unknown"];

type ElProps = Record<string, unknown> | null;
type Child = Node | string | null | false;

// Minimal hyperscript helper.
function el(tag: string, props?: ElProps, ...children: Array<Child | Child[]>): HTMLElement {
    const e = document.createElement(tag);
    if (props) {
        for (const [k, v] of Object.entries(props)) {
            if (v == null || v === false) continue;
            if (k === "class") e.className = String(v);
            else if (k === "text") e.textContent = String(v);
            else if (k === "html") e.innerHTML = String(v);
            else if (k.startsWith("on") && typeof v === "function") {
                e.addEventListener(k.slice(2), v as EventListener);
            } else if (v === true) e.setAttribute(k, "");
            else e.setAttribute(k, String(v));
        }
    }
    for (const c of children.flat()) {
        if (c == null || c === false) continue;
        e.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    }
    return e;
}

function byId(id: string): HTMLElement {
    const e = document.getElementById(id);
    if (!e) throw new Error("missing element #" + id);
    return e;
}

function avatarFor(login: string): string {
    return "https://github.com/" + encodeURIComponent(login) + ".png?size=64";
}

function fmtTime(s?: string): string {
    if (!s) return "";
    const d = new Date(s);
    if (isNaN(d.getTime())) return s;
    const diff = (Date.now() - d.getTime()) / 1000;
    if (diff < 60) return "just now";
    if (diff < 3600) return Math.floor(diff / 60) + "m ago";
    if (diff < 86400) return Math.floor(diff / 3600) + "h ago";
    if (diff < 86400 * 30) return Math.floor(diff / 86400) + "d ago";
    return d.toISOString().slice(0, 10);
}

function countOf(c: Counts, k: keyof Counts): number {
    return Number(c[k]) || 0;
}

function sumCounts(c: Counts): number {
    return COUNT_FIELDS.reduce((acc, [k]) => acc + countOf(c, k), 0);
}

interface ApiError extends Error {
    status?: number;
}

async function api<T>(path: string): Promise<T> {
    if (DEMO) return demoApi<T>(path);
    const res = await fetch(path, { headers: { Accept: "application/json" }, credentials: "same-origin" });
    if (!res.ok) {
        const err: ApiError = new Error("HTTP " + res.status);
        err.status = res.status;
        throw err;
    }
    return (await res.json()) as T;
}

// ---- top-bar user box ----
function renderUserBox(me: Me): void {
    const box = byId("user-box");
    box.innerHTML = "";
    if (!me || !me.authenticated || !me.login) {
        box.hidden = true;
        return;
    }
    box.hidden = false;
    box.appendChild(el("img", { class: "avatar", src: avatarFor(me.login), alt: "" }));
    box.appendChild(
        el("div", { class: "who" },
            el("span", { class: "login", text: me.login }),
            el("span", { class: "role", text: me.is_admin ? "Administrator" : "Signed in" }),
        ),
    );
    box.appendChild(el("button", { class: "btn btn-ghost", onclick: doLogout }, "Sign out"));
}

function doLogout(): void {
    if (DEMO) { demoSetState("logged-out"); return; }
    fetch("/logout", { method: "POST", credentials: "same-origin" }).finally(() => location.reload());
}

// ---- login / hero ----
function renderLogin(me: Me): void {
    const main = byId("main");
    main.innerHTML = "";
    const configured = !me || me.login_configured !== false;
    const onLogin = DEMO ? (() => demoSetState("user")) : null;
    main.appendChild(
        el("div", { class: "hero" },
            el("h1", { text: "Inspect your cache" }),
            el("p", { text: "Sign in with GitHub to see what the mirror has cached for your account — counts, freshness, and recent refresh activity." }),
            configured
                ? el("a", { class: "btn btn-primary", href: DEMO ? "javascript:void 0" : "/login", onclick: onLogin }, githubIcon(), "Sign in with GitHub")
                : el("button", { class: "btn btn-primary", disabled: true }, githubIcon(), "Sign in with GitHub"),
            configured
                ? el("div", { class: "note", text: "You only ever see cache scopes that your own tokens populated." })
                : el("div", { class: "note", text: "Login is not configured on this server (set GITHUB_OAUTH_CLIENT_ID / _SECRET)." }),
        ),
    );
}

function githubIcon(): Node {
    const span = el("span");
    span.innerHTML =
        '<svg class="gh-icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"></path></svg>';
    return span.firstChild as Node;
}

// ---- dashboard ----
async function renderDashboard(me: Me): Promise<void> {
    const main = byId("main");
    main.innerHTML = "";

    const head = el("div", { class: "page-head" });
    head.appendChild(
        el("div", null,
            el("h1", { text: "Cache state" }),
            el("div", { class: "sub", id: "scope-sub" }),
        ),
    );
    let tabs: HTMLElement | null = null;
    if (me.is_admin) {
        tabs = el("div", { class: "tabs" },
            el("button", { class: "active", "data-scope": "mine", onclick: () => switchTab("mine") }, "My cache"),
            el("button", { "data-scope": "all", onclick: () => switchTab("all") }, "All scopes"),
        );
        head.appendChild(tabs);
    }
    main.appendChild(head);

    const body = el("div", { id: "scope-body" });
    body.appendChild(el("div", { class: "loading", text: "Loading cache…" }));
    main.appendChild(body);

    await loadScope("mine");

    function switchTab(scope: string): void {
        if (!tabs) return;
        for (const b of Array.from(tabs.querySelectorAll("button"))) {
            b.classList.toggle("active", (b as HTMLElement).dataset.scope === scope);
        }
        void loadScope(scope);
    }
}

async function loadScope(scope: string): Promise<void> {
    const body = byId("scope-body");
    const sub = byId("scope-sub");
    body.innerHTML = "";
    body.appendChild(el("div", { class: "loading", text: "Loading cache…" }));
    let data: CacheResponse;
    try {
        data = await api<CacheResponse>("/api/cache?scope=" + scope);
    } catch (e) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load cache stats: " + (e as Error).message }));
        return;
    }
    body.innerHTML = "";
    const scopes = data.scopes ?? [];

    if (scope === "all") {
        sub.textContent = data.scope_count + " cache scope" + (data.scope_count === 1 ? "" : "s") + " across all users";
        body.appendChild(totalsGrid(data.totals, data.scope_count));
        body.appendChild(adminTable(scopes));
        return;
    }

    sub.textContent = "Everything your tokens have populated" +
        (data.scope_count ? " (" + data.scope_count + " token scope" + (data.scope_count === 1 ? "" : "s") + ")" : "");

    if (scopes.length === 0) {
        body.appendChild(el("div", { class: "empty" },
            el("p", { text: "Nothing cached yet for " + data.login + "." }),
            el("p", { class: "sub", text: "Use a tool that queries this mirror with your GitHub token, then refresh." }),
        ));
        return;
    }

    body.appendChild(totalsGrid(data.totals, data.scope_count));
    for (const s of scopes) body.appendChild(scopeCard(s, true));
}

function totalsGrid(totals: Counts, scopeCount: number): HTMLElement {
    const grid = el("div", { class: "stat-grid" });
    grid.appendChild(statCard(scopeCount, scopeCount === 1 ? "Scope" : "Scopes"));
    for (const [k, label] of COUNT_FIELDS) {
        grid.appendChild(statCard(countOf(totals, k), label));
    }
    return grid;
}

function statCard(n: number, label: string): HTMLElement {
    return el("div", { class: "stat" },
        el("div", { class: "num", text: String(n) }),
        el("div", { class: "label", text: label }),
    );
}

function scopeCard(s: ScopeStats, detailed: boolean): HTMLElement {
    const head = el("div", { class: "scope-head" });
    const id = el("div", { class: "scope-id" });
    const known = !!s.login && s.login !== "(unknown)";
    if (known) id.appendChild(el("img", { class: "avatar", src: avatarFor(s.login), alt: "" }));
    id.appendChild(el("span", { class: known ? "login" : "login unknown", text: s.login || "(unknown)" }));
    if (s.is_self) id.appendChild(el("span", { class: "badge you", text: "you" }));
    head.appendChild(id);

    const meta = el("div", { class: "scope-meta" });
    meta.appendChild(el("div", { class: "fingerprint", text: "scope " + s.actor }));
    if (s.last_seen) meta.appendChild(el("div", { text: "last seen " + fmtTime(s.last_seen) }));
    head.appendChild(meta);

    const body = el("div", { class: "scope-body" });

    const mini = el("div", { class: "mini-grid" });
    for (const [k, label] of COUNT_FIELDS) {
        const n = countOf(s.counts, k);
        mini.appendChild(el("div", { class: n === 0 ? "mini zero" : "mini" },
            el("div", { class: "n", text: String(n) }),
            el("div", { class: "l", text: label }),
        ));
    }
    body.appendChild(mini);

    const kinds = s.kinds ?? [];
    if (kinds.length) {
        body.appendChild(el("div", { class: "section-label", text: "Freshness by resource" }));
        body.appendChild(kindsTable(kinds));
    }

    if (detailed && s.recent && s.recent.length) {
        body.appendChild(el("div", { class: "section-label", text: "Recent refreshes" }));
        body.appendChild(recentList(s.recent));
    }

    return el("div", { class: "scope" }, head, body);
}

function kindsTable(kinds: KindFreshness[]): HTMLElement {
    const rows = kinds.map((k) => {
        const pills = el("td");
        for (const st of STATE_ORDER) {
            const v = k.states ? k.states[st] : 0;
            if (v) pills.appendChild(el("span", { class: "pill " + st, text: st + " " + v }));
        }
        return el("tr", null,
            el("td", { class: "kind", text: k.kind }),
            pills,
            el("td", { text: k.last_fetched ? fmtTime(k.last_fetched) : "—" }),
        );
    });
    return el("table", { class: "kinds" },
        el("thead", null, el("tr", null,
            el("th", { text: "Resource" }),
            el("th", { text: "State" }),
            el("th", { text: "Last fetched" }),
        )),
        el("tbody", null, rows),
    );
}

function recentList(recent: RecentRefresh[]): HTMLElement {
    const ul = el("ul", { class: "recent" });
    for (const r of recent) {
        ul.appendChild(el("li", null,
            el("span", { class: "dot " + (r.status || "running") }),
            el("span", { class: "r-kind", text: r.kind }),
            el("span", { class: "r-key", text: r.key }),
            el("span", { class: "r-trigger", text: r.trigger }),
            el("span", { class: "r-when", text: fmtTime(r.started_at) }),
        ));
    }
    return ul;
}

function adminTable(scopes: ScopeStats[]): HTMLElement {
    if (scopes.length === 0) {
        return el("div", { class: "empty", text: "No cache scopes recorded yet." });
    }
    const rows = scopes.map((s) => {
        const known = !!s.login && s.login !== "(unknown)";
        const loginCell = el("td", null,
            el("div", { class: "login-cell" },
                known ? el("img", { class: "avatar", src: avatarFor(s.login), alt: "" }) : null,
                el("span", { class: known ? "" : "login unknown", text: s.login || "(unknown)" }),
                s.is_self ? el("span", { class: "badge you", text: "you" }) : null,
            ),
        );
        return el("tr", null,
            loginCell,
            el("td", { class: "fingerprint", text: s.actor }),
            ...COUNT_FIELDS.map(([k]) => el("td", { class: "num", text: String(countOf(s.counts, k)) })),
            el("td", { class: "num", text: String(sumCounts(s.counts)) }),
            el("td", { text: s.last_seen ? fmtTime(s.last_seen) : "—" }),
        );
    });
    return el("table", { class: "scopes" },
        el("thead", null, el("tr", null,
            el("th", { text: "Login" }),
            el("th", { text: "Scope" }),
            ...COUNT_FIELDS.map(([, label]) => el("th", { class: "num", text: label })),
            el("th", { class: "num", text: "Total" }),
            el("th", { text: "Last seen" }),
        )),
        el("tbody", null, rows),
    );
}

// ---- boot ----
async function boot(): Promise<void> {
    if (DEMO) renderDemoBanner();
    let me: Me;
    try {
        me = await api<Me>("/api/me");
    } catch (e) {
        const main = byId("main");
        main.innerHTML = "";
        main.appendChild(el("div", { class: "error-banner", text: "Could not reach the server: " + (e as Error).message }));
        return;
    }
    renderUserBox(me);
    if (me.authenticated) {
        await renderDashboard(me);
    } else {
        renderLogin(me);
    }
}

// ---- demo mode ----
function demoApi<T>(path: string): Promise<T> {
    const cfg = DEMO as DemoConfig;
    const state = cfg.current ?? cfg.initial;
    const d = cfg.data[state] ?? ({} as DemoStateData);
    if (path === "/api/me") {
        return Promise.resolve((d.me ?? { authenticated: false, login_configured: true, is_admin: false }) as unknown as T);
    }
    if (path.startsWith("/api/cache")) {
        const payload = path.includes("scope=all") ? d.all : d.mine;
        if (!payload) {
            const err: ApiError = new Error("HTTP 401");
            err.status = 401;
            return Promise.reject(err);
        }
        return Promise.resolve(payload as unknown as T);
    }
    return Promise.reject(new Error("unknown demo path " + path));
}

function demoSetState(state: string): void {
    if (!DEMO) return;
    DEMO.current = state;
    updateDemoBanner();
    void boot();
}

let demoBannerEl: HTMLElement | null = null;
function renderDemoBanner(): void {
    const cfg = DEMO as DemoConfig;
    cfg.current = cfg.current ?? cfg.initial;
    demoBannerEl = el("div", { class: "demo-banner" },
        el("span", { html: "<strong>Demo preview</strong> — canned data, no backend. View as:" }),
        el("div", { class: "tabs" },
            demoBtn("logged-out", "Logged out"),
            demoBtn("user", "Regular user"),
            demoBtn("admin", "Admin (PazerOP)"),
        ),
    );
    document.body.insertBefore(demoBannerEl, document.body.firstChild);
    updateDemoBanner();
}

function demoBtn(state: string, label: string): HTMLElement {
    return el("button", { "data-state": state, onclick: () => demoSetState(state) }, label);
}

function updateDemoBanner(): void {
    if (!demoBannerEl || !DEMO) return;
    for (const b of Array.from(demoBannerEl.querySelectorAll("button"))) {
        b.classList.toggle("active", (b as HTMLElement).dataset.state === DEMO.current);
    }
}

if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => void boot());
} else {
    void boot();
}

export {};
