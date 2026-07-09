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
const DEMO = typeof window !== "undefined" ? window.__GSM_DEMO__ ?? null : null;
// Whether the signed-in user is an admin. Admins get the truth Browse / Check
// actions and per-principal Grants views (admin-only endpoints). Set in
// renderDashboard.
let dashIsAdmin = false;
const COUNT_FIELDS = [
    ["repos", "Repos"],
    ["pull_requests", "Pull requests"],
    ["commit_checks", "Commit checks"],
    ["contents", "Contents"],
    ["git_commits", "Git commits"],
    ["grants", "Grants"],
];
const STATE_ORDER = ["fresh", "stale", "fetching", "error", "unknown"];
// Minimal hyperscript helper.
function el(tag, props, ...children) {
    const e = document.createElement(tag);
    if (props) {
        for (const [k, v] of Object.entries(props)) {
            if (v == null || v === false)
                continue;
            if (k === "class")
                e.className = String(v);
            else if (k === "text")
                e.textContent = String(v);
            else if (k === "html")
                e.innerHTML = String(v);
            else if (k.startsWith("on") && typeof v === "function") {
                e.addEventListener(k.slice(2), v);
            }
            else if (v === true)
                e.setAttribute(k, "");
            else
                e.setAttribute(k, String(v));
        }
    }
    for (const c of children.flat()) {
        if (c == null || c === false)
            continue;
        e.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    }
    return e;
}
function byId(id) {
    const e = document.getElementById(id);
    if (!e)
        throw new Error("missing element #" + id);
    return e;
}
function avatarFor(login) {
    return "https://github.com/" + encodeURIComponent(login) + ".png?size=64";
}
function fmtTime(s) {
    if (!s)
        return "";
    const d = new Date(s);
    if (isNaN(d.getTime()))
        return s;
    const diff = (Date.now() - d.getTime()) / 1000;
    if (diff < -5) {
        // A FUTURE timestamp (e.g. a grant's Expires column) renders as
        // "in Nm/Nh/Nd" with the same buckets as the past branch. Without
        // this, every future time fell into `diff < 60` and read "just now".
        // Tiny negative skew (within 5s) still reads "just now" below.
        const ahead = -diff;
        if (ahead < 60)
            return "in " + Math.floor(ahead) + "s";
        if (ahead < 3600)
            return "in " + Math.floor(ahead / 60) + "m";
        if (ahead < 86400)
            return "in " + Math.floor(ahead / 3600) + "h";
        if (ahead < 86400 * 30)
            return "in " + Math.floor(ahead / 86400) + "d";
        return d.toISOString().slice(0, 10);
    }
    if (diff < 60)
        return "just now";
    if (diff < 3600)
        return Math.floor(diff / 60) + "m ago";
    if (diff < 86400)
        return Math.floor(diff / 3600) + "h ago";
    if (diff < 86400 * 30)
        return Math.floor(diff / 86400) + "d ago";
    return d.toISOString().slice(0, 10);
}
function countOf(c, k) {
    return Number(c[k]) || 0;
}
async function api(path, method = "GET") {
    if (DEMO)
        return demoApi(path, method);
    const res = await fetch(path, { method, headers: { Accept: "application/json" }, credentials: "same-origin" });
    if (!res.ok) {
        const err = new Error("HTTP " + res.status);
        err.status = res.status;
        throw err;
    }
    return (await res.json());
}
// ---- top-bar user box ----
function renderUserBox(me) {
    const box = byId("user-box");
    box.innerHTML = "";
    if (!me || !me.authenticated || !me.login) {
        box.hidden = true;
        return;
    }
    box.hidden = false;
    box.appendChild(el("img", { class: "avatar", src: avatarFor(me.login), alt: "" }));
    box.appendChild(el("div", { class: "who" }, el("span", { class: "login", text: me.login }), el("span", { class: "role", text: me.is_admin ? "Administrator" : "Signed in" })));
    box.appendChild(el("button", { class: "btn btn-ghost", onclick: doLogout }, "Sign out"));
}
function doLogout() {
    if (DEMO) {
        demoSetState("logged-out");
        return;
    }
    fetch("/logout", { method: "POST", credentials: "same-origin" }).finally(() => location.reload());
}
// ---- login / hero ----
function renderLogin(me) {
    const main = byId("main");
    main.innerHTML = "";
    const configured = !me || me.login_configured !== false;
    const onLogin = DEMO ? (() => demoSetState("user")) : null;
    main.appendChild(el("div", { class: "hero" }, el("h1", { text: "Inspect your cache" }), el("p", { text: "Sign in with GitHub to see what the mirror has cached for your account — counts, freshness, and recent refresh activity." }), configured
        ? el("a", { class: "btn btn-primary", href: DEMO ? "javascript:void 0" : "/login", onclick: onLogin }, githubIcon(), "Sign in with GitHub")
        : el("button", { class: "btn btn-primary", disabled: true }, githubIcon(), "Sign in with GitHub"), configured
        ? el("div", { class: "note", text: "There is one global cache; it reveals to you exactly the repos your own GitHub credentials can access." })
        : el("div", { class: "note", text: "Login is not configured on this server (set GITHUB_OAUTH_CLIENT_ID / _SECRET)." })));
}
function githubIcon() {
    const span = el("span");
    span.innerHTML =
        '<svg class="gh-icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"></path></svg>';
    return span.firstChild;
}
// ---- dashboard ----
async function renderDashboard(me) {
    dashIsAdmin = me.is_admin;
    const main = byId("main");
    main.innerHTML = "";
    const head = el("div", { class: "page-head" });
    head.appendChild(el("div", null, el("h1", { text: "Cache state" }), el("div", { class: "sub", id: "scope-sub" })));
    let tabs = null;
    if (me.is_admin) {
        tabs = el("div", { class: "tabs" }, el("button", { class: "active", "data-scope": "mine", onclick: () => switchTab("mine") }, "My cache"), el("button", { "data-scope": "all", onclick: () => switchTab("all") }, "Principals"), el("button", { "data-scope": "requests", onclick: () => switchTab("requests") }, "Requests"), el("button", { "data-scope": "webhooks", onclick: () => switchTab("webhooks") }, "Webhooks"), el("button", { "data-scope": "ratelimit", onclick: () => switchTab("ratelimit") }, "Rate limit"));
        head.appendChild(tabs);
    }
    main.appendChild(head);
    const body = el("div", { id: "scope-body" });
    body.appendChild(el("div", { class: "loading", text: "Loading cache…" }));
    main.appendChild(body);
    // Start on the tab named by the URL hash (#webhooks, #ratelimit, …) so a
    // refresh or shared link restores it — but only if that tab was actually
    // rendered for this user. An unknown (or admin-only, for a non-admin) hash
    // falls back to the default tab and is dropped from the URL.
    const fromHash = location.hash.slice(1);
    const initial = hasTab(fromHash) ? fromHash : "mine";
    if (fromHash && initial !== fromHash) {
        history.replaceState(null, "", location.pathname + location.search);
    }
    markActive(initial);
    currentView = initial;
    // Manually editing the hash (or navigating a bookmark) switches tabs too.
    // switchTab uses replaceState, which never fires hashchange — no loop.
    onHashChange = (scope) => {
        if (scope !== currentView && hasTab(scope))
            switchTab(scope);
    };
    await loadView(initial);
    startAutoRefresh();
    function hasTab(scope) {
        if (!scope || !tabs)
            return false;
        return Array.from(tabs.querySelectorAll("button"))
            .some((b) => b.dataset.scope === scope);
    }
    function markActive(scope) {
        if (!tabs)
            return;
        for (const b of Array.from(tabs.querySelectorAll("button"))) {
            b.classList.toggle("active", b.dataset.scope === scope);
        }
    }
    function switchTab(scope) {
        if (!tabs)
            return;
        markActive(scope);
        currentView = scope;
        // replaceState (not location.hash=) so tab clicks don't pile up history
        // entries the back button would have to walk through.
        history.replaceState(null, "", "#" + scope);
        void loadView(scope);
    }
}
// loadView dispatches to the loader for a tab.
function loadView(scope, silent = false) {
    if (scope === "webhooks")
        return loadWebhooks(silent);
    if (scope === "requests")
        return loadRequests(silent);
    if (scope === "ratelimit")
        return loadRateLimits(silent);
    return loadScope(scope, silent);
}
// ---- auto-refresh ----
// The dashboard polls the active tab every few seconds so cache/freshness/rate
// state stays live without a manual reload. Refreshes are "silent" (no loading
// flash) and pause while a modal is open.
const REFRESH_MS = 5000;
let currentView = "mine";
let refreshTimer;
let modalOpen = false;
// ---- URL-hash tab sync ----
// The active tab is mirrored into the URL hash so a refresh or shared link
// restores it. renderDashboard installs the handler scoped to the tabs it
// actually rendered; logged-out/error views leave it null (hash ignored).
let onHashChange = null;
if (typeof window !== "undefined") {
    window.addEventListener("hashchange", () => onHashChange?.(location.hash.slice(1)));
}
function startAutoRefresh() {
    if (DEMO || refreshTimer !== undefined)
        return;
    refreshTimer = setInterval(() => {
        if (modalOpen || document.hidden)
            return;
        void loadView(currentView, true);
    }, REFRESH_MS);
}
async function loadScope(scope, silent = false) {
    const body = byId("scope-body");
    const sub = byId("scope-sub");
    if (!silent) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "loading", text: "Loading cache…" }));
    }
    let data;
    try {
        data = await api("/api/cache?scope=" + scope);
    }
    catch (e) {
        if (silent)
            return; // keep current content during a background refresh
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load cache stats: " + e.message }));
        return;
    }
    if (currentView !== scope)
        return; // user switched tabs while we were fetching
    body.innerHTML = "";
    const principals = data.principals ?? [];
    if (scope === "all") {
        sub.textContent = "One global truth store — " + data.principal_count + " principal" +
            (data.principal_count === 1 ? "" : "s") + " with reveal-layer grants";
        body.appendChild(totalsGrid(data.totals, data.principal_count));
        if (dashIsAdmin)
            body.appendChild(truthActions());
        body.appendChild(adminTable(principals));
        return;
    }
    sub.textContent = "One global truth store — you see the slice your GitHub credentials can access";
    body.appendChild(totalsGrid(data.totals, data.principal_count));
    if (principals.length === 0) {
        body.appendChild(el("div", { class: "empty" }, el("p", { text: "No principal recorded yet for " + data.login + "." }), el("p", { class: "sub", text: "Use a tool that queries this mirror with your GitHub token, then refresh." })));
        return;
    }
    for (const s of principals)
        body.appendChild(principalCard(s, true));
}
// truthActions renders the admin actions on GLOBAL truth: browse the cached
// rows, run a read-only consistency check against GitHub, or RECONCILE --
// the apply-mode check that also corrects the drift it finds. All open the
// same modal.
function truthActions() {
    const reconcile = () => {
        if (!confirm("Reconcile rewrites global truth from GitHub: absorb missing repos/PRs, " +
            "delete stale open PRs, and correct visibility / CI rollups / auto-merge " +
            "state. Run it now?"))
            return;
        void openCheck(true);
    };
    return el("div", { class: "scope-actions", style: "margin: 0 0 12px" }, el("button", { class: "btn btn-sm", onclick: () => void openBrowse() }, "Browse truth"), el("button", { class: "btn btn-sm", onclick: () => void openCheck(false) }, "Run consistency check"), el("button", { class: "btn btn-sm", onclick: reconcile }, "Reconcile"));
}
function totalsGrid(totals, principalCount) {
    const grid = el("div", { class: "stat-grid" });
    grid.appendChild(statCard(principalCount, principalCount === 1 ? "Principal" : "Principals"));
    for (const [k, label] of COUNT_FIELDS) {
        grid.appendChild(statCard(countOf(totals, k), label));
    }
    return grid;
}
function statCard(n, label) {
    return el("div", { class: "stat" }, el("div", { class: "num", text: String(n) }), el("div", { class: "label", text: label }));
}
function principalCard(s, detailed) {
    const head = el("div", { class: "scope-head" });
    const id = el("div", { class: "scope-id" });
    const known = !!s.login && s.login !== "(unknown)";
    if (known)
        id.appendChild(el("img", { class: "avatar", src: avatarFor(s.login), alt: "" }));
    id.appendChild(el("span", { class: known ? "login" : "login unknown", text: s.login || "(unknown)" }));
    if (s.is_self)
        id.appendChild(el("span", { class: "badge you", text: "you" }));
    head.appendChild(id);
    const meta = el("div", { class: "scope-meta" });
    meta.appendChild(el("div", { class: "fingerprint", text: "principal " + s.principal }));
    if (s.last_seen)
        meta.appendChild(el("div", { text: "last seen " + fmtTime(s.last_seen) }));
    if (dashIsAdmin && s.principal_id)
        meta.appendChild(principalActions(s));
    head.appendChild(meta);
    const body = el("div", { class: "scope-body" });
    const mini = el("div", { class: "mini-grid" });
    mini.appendChild(el("div", { class: s.live_grants === 0 ? "mini zero" : "mini" }, el("div", { class: "n", text: String(s.live_grants) }), el("div", { class: "l", text: "Live repo grants" })));
    body.appendChild(mini);
    const kinds = s.kinds ?? [];
    if (kinds.length) {
        body.appendChild(el("div", { class: "section-label", text: "Org sync freshness" }));
        body.appendChild(kindsTable(kinds));
    }
    if (detailed && s.recent && s.recent.length) {
        body.appendChild(el("div", { class: "section-label", text: "Recent refreshes" }));
        body.appendChild(recentList(s.recent));
    }
    return el("div", { class: "scope" }, head, body);
}
function kindsTable(kinds) {
    const rows = [];
    for (const k of kinds) {
        const pills = el("td");
        for (const st of STATE_ORDER) {
            const v = k.states ? k.states[st] : 0;
            if (v)
                pills.appendChild(el("span", { class: "pill " + st, text: st + " " + v }));
        }
        rows.push(el("tr", null, el("td", { class: "kind", text: k.kind }), pills, el("td", { text: k.last_fetched ? fmtTime(k.last_fetched) : "—" })));
        // Show the captured failure reason for an errored kind, so the panel
        // explains *why* it errored instead of only counting it.
        if (k.error) {
            rows.push(el("tr", { class: "kind-error-row" }, el("td", { class: "kind-error", colspan: "3" }, k.error_key ? el("span", { class: "kind-error-key", text: k.error_key }) : null, el("span", { class: "kind-error-msg", text: k.error }))));
        }
    }
    return el("table", { class: "kinds" }, el("thead", null, el("tr", null, el("th", { text: "Resource" }), el("th", { text: "State" }), el("th", { text: "Last fetched" }))), el("tbody", null, rows));
}
function recentList(recent) {
    const ul = el("ul", { class: "recent" });
    for (const r of recent) {
        const li = el("li", null, el("span", { class: "dot " + (r.status || "running") }), el("span", { class: "r-kind", text: r.kind }), el("span", { class: "r-key", text: r.key }), el("span", { class: "r-trigger", text: r.trigger }), el("span", { class: "r-when", text: fmtTime(r.started_at) }));
        // Show the captured failure reason on errored refreshes.
        if (r.error) {
            li.appendChild(el("span", { class: "r-error", text: r.error }));
        }
        ul.appendChild(li);
    }
    return ul;
}
function adminTable(principals) {
    if (principals.length === 0) {
        return el("div", { class: "empty", text: "No principals recorded yet." });
    }
    const rows = principals.map((s) => {
        const known = !!s.login && s.login !== "(unknown)";
        const loginCell = el("td", null, el("div", { class: "login-cell" }, known ? el("img", { class: "avatar", src: avatarFor(s.login), alt: "" }) : null, el("span", { class: known ? "" : "login unknown", text: s.login || "(unknown)" }), s.is_self ? el("span", { class: "badge you", text: "you" }) : null));
        return el("tr", null, loginCell, el("td", { class: "fingerprint", text: s.principal }), el("td", { class: "num", text: String(s.live_grants) }), el("td", { text: s.last_seen ? fmtTime(s.last_seen) : "—" }), el("td", { class: "actions-cell" }, s.principal_id ? principalActions(s) : null));
    });
    return el("table", { class: "scopes" }, el("thead", null, el("tr", null, el("th", { text: "Login" }), el("th", { text: "Principal" }), el("th", { class: "num", text: "Live grants" }), el("th", { text: "Last seen" }), el("th", { text: "Inspect" }))), el("tbody", null, rows));
}
// principalActions renders the per-principal admin action: view the grants the
// reveal layer holds for it (who can see what).
function principalActions(s) {
    return el("div", { class: "scope-actions" }, el("button", { class: "btn btn-sm", onclick: () => void openGrants(s) }, "Grants"));
}
// ---- webhook activity (admin only) ----
async function loadWebhooks(silent = false) {
    const body = byId("scope-body");
    const sub = byId("scope-sub");
    if (!silent) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "loading", text: "Loading webhook activity…" }));
    }
    let data;
    try {
        data = await api("/api/webhooks");
    }
    catch (e) {
        if (silent)
            return;
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load webhook activity: " + e.message }));
        return;
    }
    if (currentView !== "webhooks")
        return;
    body.innerHTML = "";
    const deliveries = data.deliveries ?? [];
    sub.textContent = deliveries.length
        ? "Most recent " + deliveries.length + " webhook deliver" + (deliveries.length === 1 ? "y" : "ies") + ", newest first"
        : "Webhook deliveries and whether each one updated the cache";
    if (deliveries.length === 0) {
        body.appendChild(el("div", { class: "empty" }, el("p", { text: "No webhook deliveries recorded yet." }), el("p", { class: "sub", text: "Deliveries appear here as GitHub sends them and the mirror processes each one." })));
        return;
    }
    body.appendChild(webhookLegend());
    body.appendChild(webhookTable(deliveries));
}
const DISPOSITIONS = [
    ["applied", "state written to global truth"],
    ["invalidated", "marked stale; refetched on next read"],
    ["ignored", "event/action not tracked"],
    ["error", "internal error (GitHub retries)"],
];
function webhookLegend() {
    const legend = el("div", { class: "wh-legend" });
    for (const [disp, meaning] of DISPOSITIONS) {
        legend.appendChild(el("span", { class: "wh-legend-item" }, el("span", { class: "disp " + disp, text: disp }), el("span", { class: "wh-legend-text", text: meaning })));
    }
    return legend;
}
function webhookTable(deliveries) {
    const rows = deliveries.map((d) => {
        const disp = d.disposition || "ignored";
        const evt = d.action ? d.event_type + "." + d.action : d.event_type;
        const shortID = d.delivery_id ? d.delivery_id.slice(0, 8) : "—";
        return el("tr", null, el("td", null, el("span", { class: "disp " + disp, text: disp })), el("td", { class: "wh-event", text: evt }), el("td", { class: "wh-repo", text: d.repo || "—" }), el("td", { class: "wh-detail", text: d.detail || "" }), el("td", { class: "wh-delivery", title: d.delivery_id || "", text: shortID }), el("td", { class: "wh-when", text: fmtTime(d.received_at) }));
    });
    return el("table", { class: "webhooks" }, el("thead", null, el("tr", null, el("th", { text: "Result" }), el("th", { text: "Event" }), el("th", { text: "Repo" }), el("th", { text: "Detail" }), el("th", { text: "Delivery" }), el("th", { text: "Received" }))), el("tbody", null, rows));
}
// ---- request activity (admin only) ----
// Shows data-API requests hitting the cache and how each was served: a cache
// HIT (no GitHub call), a MISS (fetched then cached), or a PASSTHROUGH (forwarded
// to GitHub uncached). This is the live view of how much the cache is actually
// used vs. proxied straight through.
async function loadRequests(silent = false) {
    const body = byId("scope-body");
    const sub = byId("scope-sub");
    if (!silent) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "loading", text: "Loading request activity…" }));
    }
    let data;
    try {
        data = await api("/api/requests");
    }
    catch (e) {
        if (silent)
            return;
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load request activity: " + e.message }));
        return;
    }
    if (currentView !== "requests")
        return;
    body.innerHTML = "";
    const recent = data.recent ?? [];
    const by = data.by_disposition ?? {};
    const total = data.total || 0;
    sub.textContent = total + " request" + (total === 1 ? "" : "s") + " since restart" +
        " — hit " + (by.hit || 0) + pctLabel(by.hit, total) +
        ", miss " + (by.miss || 0) + pctLabel(by.miss, total) +
        ", passthrough " + (by.passthrough || 0) + pctLabel(by.passthrough, total) +
        ", write " + (by.write || 0) + pctLabel(by.write, total) +
        (by.error ? ", error " + by.error + pctLabel(by.error, total) : "");
    body.appendChild(requestLegend());
    if (recent.length === 0) {
        body.appendChild(el("div", { class: "empty" }, el("p", { text: "No data-API requests recorded yet." }), el("p", { class: "sub", text: "Requests appear here live as clients call the mirror — cache hits, misses, and passthroughs to GitHub." })));
        return;
    }
    body.appendChild(requestTable(recent));
}
// A count's share of the total, rendered as a " (88.8%)" suffix — empty when
// there is no total to take a share of.
function pctLabel(count, total) {
    return total > 0 ? " (" + (((count || 0) / total) * 100).toFixed(1) + "%)" : "";
}
const REQUEST_DISPOSITIONS = [
    ["hit", "served from cache, no GitHub call"],
    ["miss", "cache miss; fetched from GitHub then cached"],
    ["passthrough", "read forwarded to GitHub uncached"],
    ["write", "mutation proxied to GitHub (never cacheable)"],
    ["error", "cache lookup/fetch failed"],
];
// reqDispClass maps a request disposition onto the existing webhook chip colors,
// so the Requests view is styled without new CSS.
function reqDispClass(disp) {
    switch (disp) {
        case "hit": return "applied";
        case "miss": return "invalidated";
        case "passthrough": return "ignored";
        case "write": return "write";
        default: return "error";
    }
}
function requestLegend() {
    const legend = el("div", { class: "wh-legend" });
    for (const [disp, meaning] of REQUEST_DISPOSITIONS) {
        legend.appendChild(el("span", { class: "wh-legend-item" }, el("span", { class: "disp " + reqDispClass(disp), text: disp }), el("span", { class: "wh-legend-text", text: meaning })));
    }
    return legend;
}
function requestTable(events) {
    const rows = events.map((e) => {
        const disp = e.disposition || "passthrough";
        return el("tr", null, el("td", null, el("span", { class: "disp " + reqDispClass(disp), text: disp })), el("td", null, statusBadge(e.status)), el("td", { class: "wh-event", text: e.method }), el("td", { class: "wh-repo", text: e.path }), el("td", { class: "wh-detail", text: e.actor || "" }), el("td", { class: "wh-when", text: fmtTime(e.at) }));
    });
    return el("table", { class: "webhooks" }, el("thead", null, el("tr", null, el("th", { text: "Result" }), el("th", { text: "Upstream" }), el("th", { text: "Method" }), el("th", { text: "Path" }), el("th", { text: "Caller" }), el("th", { text: "When" }))), el("tbody", null, rows));
}
// statusBadge renders the upstream HTTP status for a passthrough, colored by
// class (2xx ok, 3xx redirect, 4xx/5xx error). Empty when there's no upstream
// call (a cache hit), where "—" is shown instead.
function statusBadge(status) {
    if (!status)
        return document.createTextNode("—");
    const cls = status >= 500 ? "err" : status >= 400 ? "warn" : status >= 300 ? "redir" : "ok";
    return el("span", { class: "status-code " + cls, text: String(status) });
}
// ---- GitHub rate limit (admin only) ----
// Two halves: "live" — the mirror's own GitHub App polled per installation —
// and "observed" — X-RateLimit-* headers passively recorded off every
// upstream response, covering the callers' credentials too, at zero API cost.
const RATE_RESOURCES = ["core", "graphql", "search"];
async function loadRateLimits(silent = false) {
    const body = byId("scope-body");
    const sub = byId("scope-sub");
    if (!silent) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "loading", text: "Loading rate limit…" }));
    }
    let data;
    try {
        data = await api("/api/ratelimit");
    }
    catch (e) {
        if (silent)
            return;
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load rate limit: " + e.message }));
        return;
    }
    if (currentView !== "ratelimit")
        return;
    body.innerHTML = "";
    const installs = data.live ?? [];
    // Zero-usage observations (used <= 0 — nothing consumed in the current
    // window, e.g. an identity that only ever got 304s) are noise; hide them
    // client-side. If everything filters out, the section's normal empty
    // state below renders instead of an empty grid.
    const observed = (data.observed ?? []).filter((o) => o.used > 0);
    sub.textContent = "GitHub rate limits — the App's live per-installation poll" +
        (installs.length ? " (" + installs.length + " installation" + (installs.length === 1 ? "" : "s") + ")" : "") +
        ", plus readings observed passively from response headers";
    // A missing App / failed poll arrives as a note, not an error: the
    // observed half below still renders.
    if (data.note) {
        body.appendChild(el("div", { class: "notes" }, el("div", { class: "note-line", text: data.note })));
    }
    if (installs.length) {
        body.appendChild(el("div", { class: "section-label", text: "Live — GitHub App installations (polled)" }));
        for (const inst of installs)
            body.appendChild(rateLimitCard(inst));
    }
    else if (!data.note) {
        body.appendChild(el("div", { class: "empty" }, el("p", { text: "No GitHub App installations found." }), el("p", { class: "sub", text: "The App is configured but has no installations." })));
    }
    body.appendChild(el("div", { class: "section-label", text: "Observed from response headers" }));
    if (observed.length) {
        body.appendChild(observedRateGrid(observed));
    }
    else {
        body.appendChild(el("div", { class: "empty" }, el("p", { text: "No rate-limit headers observed yet." }), el("p", { class: "sub", text: "Every upstream GitHub response's X-RateLimit-* headers are recorded here per identity as the mirror serves traffic. In-memory; resets on restart." })));
    }
}
// observedRateGrid renders one tile per (identity, resource). The backend
// sorts by identity then resource, so one identity's buckets sit together;
// each tile is a <rate-meter> plus an "observed …" caption.
function observedRateGrid(observed) {
    const grid = el("div", { class: "rate-grid" });
    for (const o of observed) {
        grid.appendChild(el("div", { class: "rate-observed" }, el("rate-meter", {
            name: o.identity + " — " + o.resource,
            limit: o.limit, remaining: o.remaining, used: o.used, reset: o.reset,
        }), el("div", { class: "rate-observed-at", text: "observed " + fmtTime(o.observed_at) })));
    }
    return grid;
}
function rateLimitCard(inst) {
    const head = el("div", { class: "scope-head" }, el("div", { class: "scope-id" }, el("img", { class: "avatar", src: avatarFor(inst.installation), alt: "" }), el("span", { class: "login", text: inst.installation }), inst.account_type ? el("span", { class: "badge", text: inst.account_type }) : null));
    const body = el("div", { class: "scope-body" });
    if (inst.error) {
        body.appendChild(el("div", { class: "error-banner", text: inst.error }));
        return el("div", { class: "scope" }, head, body);
    }
    const resources = inst.resources ?? {};
    const names = RATE_RESOURCES.filter((n) => resources[n]).concat(Object.keys(resources).filter((n) => !RATE_RESOURCES.includes(n)));
    const grid = el("div", { class: "rate-grid" });
    // Each tile is a <rate-meter> web component (src/rate-meter.ts, loaded by
    // its own <script> tag in index.html) that owns its markup and styles in
    // shadow DOM; el() passes these unknown props through as attributes.
    for (const name of names) {
        const r = resources[name];
        grid.appendChild(el("rate-meter", {
            name, limit: r.limit, remaining: r.remaining, used: r.used, reset: r.reset,
        }));
    }
    body.appendChild(grid);
    return el("div", { class: "scope" }, head, body);
}
// ---- admin: browse + consistency check (modal) ----
function principalLabel(s) {
    return s.login && s.login !== "(unknown)" ? s.login : "principal " + s.principal;
}
// openModal creates a dismissable overlay and returns its (empty) body element.
function openModal(titleText) {
    const body = el("div", { class: "modal-body" });
    const closeBtn = el("button", { class: "btn btn-ghost modal-close", title: "Close" }, "✕");
    const card = el("div", { class: "modal-card" }, el("div", { class: "modal-head" }, el("div", { class: "modal-title", text: titleText }), closeBtn), body);
    const backdrop = el("div", { class: "modal-backdrop" }, card);
    modalOpen = true; // pause auto-refresh while the modal is open
    const close = () => {
        backdrop.remove();
        modalOpen = false;
        document.removeEventListener("keydown", onKey);
    };
    function onKey(e) { if (e.key === "Escape")
        close(); }
    closeBtn.addEventListener("click", close);
    backdrop.addEventListener("click", (e) => { if (e.target === backdrop)
        close(); });
    document.addEventListener("keydown", onKey);
    document.body.appendChild(backdrop);
    return { body, close };
}
// jsonBlock renders a pretty-printed, copyable JSON view. This is the artifact
// the operator pastes back for analysis.
function jsonBlock(obj, copyLabel) {
    const text = JSON.stringify(obj, null, 2);
    const copyBtn = el("button", { class: "btn btn-sm" }, copyLabel);
    copyBtn.addEventListener("click", () => {
        const done = (ok) => {
            copyBtn.textContent = ok ? "Copied!" : "Copy failed";
            setTimeout(() => { copyBtn.textContent = copyLabel; }, 1500);
        };
        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(() => done(true), () => done(false));
        }
        else {
            done(false);
        }
    });
    return el("div", { class: "json-wrap" }, el("div", { class: "json-toolbar" }, copyBtn), el("pre", { class: "json-block", text }));
}
async function openBrowse() {
    const { body } = openModal("Global truth — cached rows");
    body.appendChild(el("div", { class: "loading", text: "Loading cached rows…" }));
    let data;
    try {
        data = await api("/api/cache/data");
    }
    catch (e) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load cache contents: " + e.message }));
        return;
    }
    body.innerHTML = "";
    body.appendChild(renderBrowse(data));
}
function renderBrowse(d) {
    const wrap = el("div", { class: "detail" });
    wrap.appendChild(el("div", { class: "detail-sub", text: "The one global truth store — what webhooks and principals' fetches have absorbed" }));
    const grid = el("div", { class: "stat-grid" });
    for (const [k, label] of COUNT_FIELDS)
        grid.appendChild(statCard(countOf(d.counts, k), label));
    wrap.appendChild(grid);
    if (d.repos.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Repositories (" + d.repos.length + ")" }));
        wrap.appendChild(browseRepoTable(d.repos));
    }
    if (d.pull_requests.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Pull requests (" + d.pull_requests.length + ")" }));
        wrap.appendChild(browsePRTable(d.pull_requests));
    }
    if (d.commit_checks.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Also cached" }));
        wrap.appendChild(el("div", { class: "chips" }, el("span", { class: "chip" }, "Commit checks " + d.commit_checks.length)));
    }
    const raw = el("details", { class: "raw" }, el("summary", { text: "Raw JSON (every cached truth row)" }), jsonBlock(d, "Copy JSON"));
    wrap.appendChild(raw);
    return wrap;
}
// openGrants shows one principal's reveal-layer grants: exactly which repos
// the cache will reveal to it, and where each grant came from.
async function openGrants(s) {
    const principalId = s.principal_id;
    if (!principalId)
        return;
    const { body } = openModal("Access grants — " + principalLabel(s));
    body.appendChild(el("div", { class: "loading", text: "Loading grants…" }));
    let data;
    try {
        data = await api("/api/cache/data?principal=" + encodeURIComponent(principalId));
    }
    catch (e) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load grants: " + e.message }));
        return;
    }
    body.innerHTML = "";
    body.appendChild(renderGrants(data));
}
function renderGrants(d) {
    const wrap = el("div", { class: "detail" });
    wrap.appendChild(el("div", { class: "detail-sub", text: "Principal " + d.principal_id + (d.login ? " · " + d.login : "") +
            " — repos the reveal layer will serve to it (public repos need no grant)" }));
    const grants = d.grants ?? [];
    if (grants.length === 0) {
        wrap.appendChild(el("div", { class: "empty", text: "No live grants. This principal has only public-repo (or no) access so far." }));
    }
    else {
        wrap.appendChild(el("div", { class: "section-label", text: "Grants (" + grants.length + ")" }));
        wrap.appendChild(grantsTable(grants));
    }
    const raw = el("details", { class: "raw" }, el("summary", { text: "Raw JSON" }), jsonBlock(d, "Copy JSON"));
    wrap.appendChild(raw);
    return wrap;
}
function grantsTable(grants) {
    const rows = grants.map((g) => el("tr", null, el("td", { class: "wh-repo", text: g.owner + "/" + g.repo }), el("td", null, el("span", { class: "pill fresh", text: g.source })), el("td", { text: fmtTime(g.granted_at) }), el("td", { text: fmtTime(g.expires_at) })));
    return el("table", { class: "kinds detail-table" }, el("thead", null, el("tr", null, el("th", { text: "Repo" }), el("th", { text: "Source" }), el("th", { text: "Granted" }), el("th", { text: "Expires" }))), el("tbody", null, rows));
}
function browseRepoTable(repos) {
    const rows = repos.map((r) => el("tr", null, el("td", null, el("a", { href: r.url, target: "_blank", rel: "noopener", text: r.name_with_owner })), el("td", { text: r.visibility || "unknown" }), el("td", { text: r.default_branch || "—" }), statusCell(r.default_branch_status), el("td", { text: r.is_archived ? "archived" : r.is_disabled ? "disabled" : "active" })));
    return el("table", { class: "kinds detail-table" }, el("thead", null, el("tr", null, el("th", { text: "Repo" }), el("th", { text: "Visibility" }), el("th", { text: "Default branch" }), el("th", { text: "Branch status" }), el("th", { text: "Flags" }))), el("tbody", null, rows));
}
function browsePRTable(prs) {
    const rows = prs.map((p) => el("tr", null, el("td", null, el("a", { href: p.url, target: "_blank", rel: "noopener", text: p.owner + "/" + p.repo + " #" + p.number })), el("td", { class: "pr-title", text: p.title }), el("td", { text: p.is_draft ? "draft" : p.state.toLowerCase() }), statusCell(p.last_commit_status), el("td", null, ...(p.labels ?? []).map((l) => el("span", { class: "label-chip", text: l })))));
    return el("table", { class: "kinds detail-table" }, el("thead", null, el("tr", null, el("th", { text: "PR" }), el("th", { text: "Title" }), el("th", { text: "State" }), el("th", { text: "CI" }), el("th", { text: "Labels" }))), el("tbody", null, rows));
}
function statusCell(status) {
    if (!status)
        return el("td", { text: "—" });
    return el("td", null, el("span", { class: "status-pill " + status.toLowerCase(), text: status }));
}
// ---- streaming consistency check / reconcile ----
//
// The endpoint's ?stream=1 mode answers NDJSON: one JSON progress line per
// checker phase (a fleet run takes minutes — per owner: a paginated repo
// fetch, a visibility fetch, the diff, and in apply mode the corrections),
// with the full report as the final {"phase":"report"} line. EventSource
// can't POST (the Reconcile is a POST), so the body stream is read manually.
// splitNdjson consumes the COMPLETE lines in buf, returning them plus the
// unconsumed remainder (a partial trailing line). Pure — the stream reader
// feeds it chunk by chunk.
function splitNdjson(buf) {
    const parts = buf.split("\n");
    const rest = parts.pop() ?? "";
    return { lines: parts.filter((l) => l.trim() !== ""), rest };
}
// reduceCheckProgress folds one progress event into the render state. Pure,
// and tolerant of arbitrary event subsets (the demo replay is abbreviated).
function reduceCheckProgress(p, ev) {
    const next = { ...p };
    const who = ev.owner ?? "";
    switch (ev.phase) {
        case "start":
            next.ownersTotal = ev.owners ?? 0;
            next.status = next.ownersTotal > 0
                ? "starting: " + next.ownersTotal + " owner" + (next.ownersTotal === 1 ? "" : "s") + " to check…"
                : "starting…";
            break;
        case "owner":
            next.ownerIndex = ev.index ?? p.ownerIndex + 1;
            if (ev.total)
                next.ownersTotal = ev.total;
            next.within = 0.05;
            next.status = who + ": fetching repos…";
            break;
        case "fetch": {
            const n = ev.repos_fetched ?? 0;
            const total = ev.repos_total ?? 0;
            // The paginated repo fetch dominates an owner's wall time; scale it
            // across most of the owner's slice when the total is known.
            next.within = total > 0 ? 0.05 + 0.75 * Math.min(n / total, 1) : 0.4;
            next.status = who + ": fetched " + n + (total > 0 ? "/" + total : "") + " repos…";
            break;
        }
        case "visibility":
            next.within = 0.85;
            next.status = who + ": fetching repo visibility…";
            break;
        case "diffed": {
            const d = ev.discrepancies ?? 0;
            next.within = 0.95;
            next.status = who + ": diffed — " + d + (d === 1 ? " discrepancy" : " discrepancies") + " so far";
            break;
        }
        case "applied":
            next.within = 1;
            next.status = who + ": corrections applied";
            break;
        case "skip":
            next.within = 1;
            next.status = who + ": skipped — " + (ev.reason ?? "");
            break;
        case "done":
            next.ownerIndex = next.ownersTotal;
            next.within = 1;
            next.status = "finalizing report…";
            break;
    }
    return next;
}
// checkProgressFraction maps the state to the bar's 0..1 fill: owners
// completed plus the within-owner sub-progress. Pure.
function checkProgressFraction(p) {
    if (p.ownersTotal <= 0)
        return 0;
    const f = (Math.max(p.ownerIndex, 1) - 1 + p.within) / p.ownersTotal;
    return Math.max(0, Math.min(1, f));
}
// runCheckStream drives the streaming run, invoking onEvent per progress line
// and resolving with the final report (rejecting on HTTP or run errors).
async function runCheckStream(apply, onEvent) {
    if (DEMO)
        return demoCheckStream(apply, onEvent);
    const res = await fetch("/api/cache/check?stream=1" + (apply ? "&apply=true" : ""), {
        method: apply ? "POST" : "GET",
        headers: { Accept: "application/x-ndjson" },
        credentials: "same-origin",
    });
    if (!res.ok) {
        const err = new Error("HTTP " + res.status);
        err.status = res.status;
        throw err;
    }
    let report = null;
    let runError = "";
    const handle = (line) => {
        let ev;
        try {
            ev = JSON.parse(line);
        }
        catch {
            return; // tolerate a garbled line; the terminal line decides the outcome
        }
        if (ev.phase === "report" && ev.report)
            report = ev.report;
        else if (ev.phase === "error")
            runError = ev.error || "check failed";
        else
            onEvent(ev);
    };
    if (!res.body) {
        // No ReadableStream support: the whole run arrives as one buffered body.
        for (const line of splitNdjson((await res.text()) + "\n").lines)
            handle(line);
    }
    else {
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buf = "";
        for (;;) {
            const { done, value } = await reader.read();
            if (value)
                buf += decoder.decode(value, { stream: true });
            if (done)
                buf += decoder.decode() + "\n"; // flush + terminate the last line
            const split = splitNdjson(buf);
            buf = split.rest;
            for (const line of split.lines)
                handle(line);
            if (done)
                break;
        }
    }
    if (runError)
        throw new Error(runError);
    if (!report)
        throw new Error("progress stream ended without a report");
    return report;
}
// demoCheckStream replays the canned progress sequence on a timer (the demo
// mock can't stream), then resolves the canned report — so the preview
// exercises the live progress UI end to end.
function demoCheckStream(apply, onEvent) {
    const cfg = DEMO;
    const state = cfg.current ?? cfg.initial;
    const d = cfg.data[state] ?? {};
    const report = apply ? d.checkApplied : d.check;
    if (!report)
        return demoReject(503);
    const seq = d.checkProgress ?? [];
    return new Promise((resolve) => {
        let i = 0;
        const step = () => {
            if (i < seq.length) {
                onEvent(seq[i++]);
                setTimeout(step, 250);
            }
            else {
                resolve(report);
            }
        };
        step();
    });
}
async function openCheck(apply) {
    const { body } = openModal(apply
        ? "Reconcile — correct global truth from GitHub"
        : "Consistency check — global truth vs GitHub");
    // Live progress while the run streams: a bar (owners completed / total,
    // refined by within-owner repo pages) + one status line. A full fleet run
    // takes minutes, not seconds.
    let state = {
        ownersTotal: 0, ownerIndex: 0, within: 0,
        status: apply
            ? "Fetching source of truth from GitHub, diffing, and correcting the drift…"
            : "Fetching source of truth from GitHub and diffing…",
    };
    const fill = el("div", { class: "check-progress-fill" });
    const statusLine = el("div", { class: "check-progress-status", text: state.status });
    const counter = el("div", { class: "check-progress-count" });
    body.appendChild(el("div", { class: "check-progress" }, el("div", { class: "check-progress-track" }, fill), el("div", { class: "check-progress-meta" }, statusLine, counter)));
    const onEvent = (ev) => {
        state = reduceCheckProgress(state, ev);
        fill.style.width = (checkProgressFraction(state) * 100).toFixed(1) + "%";
        statusLine.textContent = state.status;
        counter.textContent = state.ownersTotal > 0
            ? Math.min(Math.max(state.ownerIndex, 1), state.ownersTotal) + "/" + state.ownersTotal + " owners"
            : "";
    };
    let report;
    try {
        report = await runCheckStream(apply, onEvent);
    }
    catch (e) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: (apply ? "Reconcile" : "Consistency check") + " failed: " + e.message }));
        return;
    }
    body.innerHTML = "";
    body.appendChild(renderCheck(report));
}
function renderCheck(r) {
    const wrap = el("div", { class: "detail" });
    wrap.appendChild(el("div", { class: "detail-sub", text: "Fetched as " + r.fetched_as + " · " + r.orgs_checked.length + " org" +
            (r.orgs_checked.length === 1 ? "" : "s") + " checked · " + fmtTime(r.generated_at) }));
    // Headline: the copyable JSON is the whole point, so surface it first.
    wrap.appendChild(jsonBlock(r, "Copy report JSON"));
    const sm = r.summary;
    const grid = el("div", { class: "stat-grid" });
    grid.appendChild(statCard(sm.discrepancies, "Discrepancies"));
    grid.appendChild(statCard(sm.repos_only_in_cache, "Repos only cached"));
    grid.appendChild(statCard(sm.repos_only_in_cache_archived ?? 0, "…of those, archived (expected)"));
    grid.appendChild(statCard(sm.repos_only_on_github, "Repos only on GH"));
    grid.appendChild(statCard(sm.repos_only_on_github_private, "…of those, private (lazy truth)"));
    grid.appendChild(statCard(sm.prs_only_in_cache, "PRs only cached"));
    grid.appendChild(statCard(sm.prs_only_on_github, "PRs only on GH"));
    grid.appendChild(statCard(sm.field_mismatches, "Field mismatches"));
    grid.appendChild(statCard(sm.visibility_leaks ?? 0, "Visibility leaks"));
    wrap.appendChild(grid);
    const tf = r.truth_freshness ?? {};
    const owners = Object.keys(tf).sort();
    if (owners.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Truth freshness (most recent org sync per owner)" }));
        wrap.appendChild(truthFreshnessTable(owners.map((o) => [o, tf[o]])));
    }
    // The Reconcile tally: what apply mode actually corrected (discrepancies
    // below show the PRE-apply state).
    if (r.applied) {
        wrap.appendChild(el("div", { class: "section-label", text: "Applied corrections" }));
        wrap.appendChild(appliedGrid(r.applied));
    }
    if (r.discrepancies.length === 0) {
        wrap.appendChild(el("div", { class: "ok-banner", text: "No drift detected. The cache matches GitHub for every org checked." }));
    }
    else {
        wrap.appendChild(el("div", { class: "section-label", text: "Discrepancies (" + r.discrepancies.length + ")" }));
        wrap.appendChild(discrepancyTable(r.discrepancies));
    }
    if (r.orgs_skipped && r.orgs_skipped.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Skipped owners" }));
        const ul = el("ul", { class: "recent" });
        for (const o of r.orgs_skipped) {
            ul.appendChild(el("li", null, el("span", { class: "r-kind", text: o.org }), el("span", { class: "r-key", text: o.reason })));
        }
        wrap.appendChild(ul);
    }
    if (r.notes && r.notes.length) {
        const notes = el("div", { class: "notes" });
        for (const n of r.notes)
            notes.appendChild(el("div", { class: "note-line", text: n }));
        wrap.appendChild(notes);
    }
    return wrap;
}
// appliedGrid renders the Reconcile tally as stat tiles.
function appliedGrid(a) {
    const grid = el("div", { class: "stat-grid" });
    const tiles = [
        [a.repos_absorbed, "Repos absorbed"],
        [a.prs_absorbed, "PRs absorbed"],
        [a.prs_deleted, "Stale PRs deleted"],
        [a.visibility_set, "Visibility set"],
        [a.statuses_corrected, "Statuses corrected"],
        [a.check_rows_deleted, "Check rows deleted"],
        [a.default_branch_status_set, "Branch statuses set"],
        [a.auto_merge_set, "Auto-merge set"],
    ];
    for (const [n, label] of tiles)
        grid.appendChild(statCard(n, label));
    return grid;
}
function discrepancyTable(items) {
    const rows = items.map((d) => {
        const where = d.kind === "pr" ? d.repo + " #" + (d.pr ?? "") : d.repo;
        const noteBits = [];
        if (d.title)
            noteBits.push("“" + d.title + "”");
        if (d.note)
            noteBits.push(d.note);
        if (d.fix)
            noteBits.push("fix: " + d.fix);
        return el("tr", null, el("td", null, el("span", { class: "disp issue-" + d.issue.replace(/_/g, "-"), text: d.issue.replace(/_/g, " ") })), el("td", { class: "wh-repo", text: where }), el("td", { class: "kind", text: d.field || "—" }), el("td", { class: "diff-cached", text: d.cached || "" }), el("td", { class: "diff-github", text: d.github || "" }), el("td", { class: "wh-detail" }, d.visibility ? el("span", { class: "badge", text: d.visibility }) : null, d.archived ? el("span", { class: "badge", text: "archived" }) : null, d.served_now ? el("span", { class: "badge", text: "served now" }) : null, noteBits.length ? " " + noteBits.join(" — ") : ""));
    });
    return el("table", { class: "webhooks detail-table" }, el("thead", null, el("tr", null, el("th", { text: "Issue" }), el("th", { text: "Where" }), el("th", { text: "Field" }), el("th", { text: "Cached" }), el("th", { text: "GitHub" }), el("th", { text: "Note" }))), el("tbody", null, rows));
}
function truthFreshnessTable(rows) {
    const trs = rows.map(([owner, f]) => el("tr", null, el("td", { class: "kind", text: owner }), el("td", null, el("span", { class: "pill " + (f.state || "unknown"), text: f.state || "unknown" })), el("td", { text: f.last_fetched_at ? fmtTime(f.last_fetched_at) : "—" }), el("td", { class: "fingerprint", text: f.principal || "—" }), el("td", { class: "wh-detail", text: f.error || "" })));
    return el("table", { class: "kinds detail-table" }, el("thead", null, el("tr", null, el("th", { text: "Owner" }), el("th", { text: "State" }), el("th", { text: "Last synced" }), el("th", { text: "By principal" }), el("th", { text: "Error" }))), el("tbody", null, trs));
}
// ---- boot ----
async function boot() {
    onHashChange = null; // re-installed by renderDashboard for the tabs it renders
    if (DEMO)
        renderDemoBanner();
    let me;
    try {
        me = await api("/api/me");
    }
    catch (e) {
        const main = byId("main");
        main.innerHTML = "";
        main.appendChild(el("div", { class: "error-banner", text: "Could not reach the server: " + e.message }));
        return;
    }
    renderUserBox(me);
    if (me.authenticated) {
        await renderDashboard(me);
    }
    else {
        renderLogin(me);
    }
}
// ---- demo mode ----
function demoQuery(path, key) {
    const q = path.split("?")[1] ?? "";
    return new URLSearchParams(q).get(key) ?? "";
}
function demoReject(status) {
    const err = new Error("HTTP " + status);
    err.status = status;
    return Promise.reject(err);
}
function demoApi(path, method = "GET") {
    const cfg = DEMO;
    const state = cfg.current ?? cfg.initial;
    const d = cfg.data[state] ?? {};
    if (path === "/api/me") {
        return Promise.resolve((d.me ?? { authenticated: false, login_configured: true, is_admin: false }));
    }
    if (path.startsWith("/api/cache/data")) {
        const principal = demoQuery(path, "principal");
        if (principal) {
            const g = d.grants?.[principal];
            return g ? Promise.resolve(g) : demoReject(404);
        }
        return d.browse ? Promise.resolve(d.browse) : demoReject(404);
    }
    if (path.startsWith("/api/cache/check")) {
        const applied = demoQuery(path, "apply");
        if ((applied === "true" || applied === "1") && method === "POST") {
            return d.checkApplied ? Promise.resolve(d.checkApplied) : demoReject(503);
        }
        return d.check ? Promise.resolve(d.check) : demoReject(503);
    }
    if (path.startsWith("/api/cache")) {
        const payload = path.includes("scope=all") ? d.all : d.mine;
        if (!payload)
            return demoReject(401);
        return Promise.resolve(payload);
    }
    if (path.startsWith("/api/webhooks")) {
        if (!d.webhooks) {
            const err = new Error("HTTP 403");
            err.status = 403;
            return Promise.reject(err);
        }
        return Promise.resolve(d.webhooks);
    }
    if (path.startsWith("/api/requests")) {
        if (!d.requests) {
            const err = new Error("HTTP 403");
            err.status = 403;
            return Promise.reject(err);
        }
        return Promise.resolve(d.requests);
    }
    if (path.startsWith("/api/ratelimit")) {
        if (!d.ratelimit)
            return demoReject(503);
        return Promise.resolve(d.ratelimit);
    }
    return Promise.reject(new Error("unknown demo path " + path));
}
function demoSetState(state) {
    if (!DEMO)
        return;
    DEMO.current = state;
    updateDemoBanner();
    void boot();
}
let demoBannerEl = null;
function renderDemoBanner() {
    const cfg = DEMO;
    cfg.current = cfg.current ?? cfg.initial;
    // boot() re-runs on every demo state switch; reuse the banner already in
    // the DOM instead of stacking another one — only its active button changes.
    if (demoBannerEl && demoBannerEl.isConnected) {
        updateDemoBanner();
        return;
    }
    demoBannerEl = el("div", { class: "demo-banner" }, el("span", { html: "<strong>Demo preview</strong> — canned data, no backend. View as:" }), el("div", { class: "tabs" }, demoBtn("logged-out", "Logged out"), demoBtn("user", "Regular user"), demoBtn("admin", "Admin (PazerOP)")));
    document.body.insertBefore(demoBannerEl, document.body.firstChild);
    updateDemoBanner();
}
function demoBtn(state, label) {
    return el("button", { "data-state": state, onclick: () => demoSetState(state) }, label);
}
function updateDemoBanner() {
    if (!demoBannerEl || !DEMO)
        return;
    for (const b of Array.from(demoBannerEl.querySelectorAll("button"))) {
        b.classList.toggle("active", b.dataset.state === DEMO.current);
    }
}
if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => void boot());
}
else {
    void boot();
}
export {};
