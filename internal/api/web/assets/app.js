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
// Whether the signed-in user is an admin. Admins get the per-scope Browse /
// Check actions (the data/check endpoints are admin-only). Set in renderDashboard.
let dashIsAdmin = false;
const COUNT_FIELDS = [
    ["repos", "Repos"],
    ["pull_requests", "Pull requests"],
    ["orgs", "Orgs"],
    ["users", "Users"],
    ["commit_checks", "Commit checks"],
    ["pr_files", "PR files"],
    ["branch_comparisons", "Comparisons"],
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
function sumCounts(c) {
    return COUNT_FIELDS.reduce((acc, [k]) => acc + countOf(c, k), 0);
}
async function api(path) {
    if (DEMO)
        return demoApi(path);
    const res = await fetch(path, { headers: { Accept: "application/json" }, credentials: "same-origin" });
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
        ? el("div", { class: "note", text: "You only ever see cache scopes that your own tokens populated." })
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
        tabs = el("div", { class: "tabs" }, el("button", { class: "active", "data-scope": "mine", onclick: () => switchTab("mine") }, "My cache"), el("button", { "data-scope": "all", onclick: () => switchTab("all") }, "All scopes"), el("button", { "data-scope": "webhooks", onclick: () => switchTab("webhooks") }, "Webhooks"));
        head.appendChild(tabs);
    }
    main.appendChild(head);
    const body = el("div", { id: "scope-body" });
    body.appendChild(el("div", { class: "loading", text: "Loading cache…" }));
    main.appendChild(body);
    await loadScope("mine");
    function switchTab(scope) {
        if (!tabs)
            return;
        for (const b of Array.from(tabs.querySelectorAll("button"))) {
            b.classList.toggle("active", b.dataset.scope === scope);
        }
        if (scope === "webhooks")
            void loadWebhooks();
        else
            void loadScope(scope);
    }
}
async function loadScope(scope) {
    const body = byId("scope-body");
    const sub = byId("scope-sub");
    body.innerHTML = "";
    body.appendChild(el("div", { class: "loading", text: "Loading cache…" }));
    let data;
    try {
        data = await api("/api/cache?scope=" + scope);
    }
    catch (e) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load cache stats: " + e.message }));
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
        body.appendChild(el("div", { class: "empty" }, el("p", { text: "Nothing cached yet for " + data.login + "." }), el("p", { class: "sub", text: "Use a tool that queries this mirror with your GitHub token, then refresh." })));
        return;
    }
    body.appendChild(totalsGrid(data.totals, data.scope_count));
    for (const s of scopes)
        body.appendChild(scopeCard(s, true));
}
function totalsGrid(totals, scopeCount) {
    const grid = el("div", { class: "stat-grid" });
    grid.appendChild(statCard(scopeCount, scopeCount === 1 ? "Scope" : "Scopes"));
    for (const [k, label] of COUNT_FIELDS) {
        grid.appendChild(statCard(countOf(totals, k), label));
    }
    return grid;
}
function statCard(n, label) {
    return el("div", { class: "stat" }, el("div", { class: "num", text: String(n) }), el("div", { class: "label", text: label }));
}
function scopeCard(s, detailed) {
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
    meta.appendChild(el("div", { class: "fingerprint", text: "scope " + s.actor }));
    if (s.last_seen)
        meta.appendChild(el("div", { text: "last seen " + fmtTime(s.last_seen) }));
    if (dashIsAdmin && s.actor_id)
        meta.appendChild(scopeActions(s));
    head.appendChild(meta);
    const body = el("div", { class: "scope-body" });
    const mini = el("div", { class: "mini-grid" });
    for (const [k, label] of COUNT_FIELDS) {
        const n = countOf(s.counts, k);
        mini.appendChild(el("div", { class: n === 0 ? "mini zero" : "mini" }, el("div", { class: "n", text: String(n) }), el("div", { class: "l", text: label })));
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
function kindsTable(kinds) {
    const rows = kinds.map((k) => {
        const pills = el("td");
        for (const st of STATE_ORDER) {
            const v = k.states ? k.states[st] : 0;
            if (v)
                pills.appendChild(el("span", { class: "pill " + st, text: st + " " + v }));
        }
        return el("tr", null, el("td", { class: "kind", text: k.kind }), pills, el("td", { text: k.last_fetched ? fmtTime(k.last_fetched) : "—" }));
    });
    return el("table", { class: "kinds" }, el("thead", null, el("tr", null, el("th", { text: "Resource" }), el("th", { text: "State" }), el("th", { text: "Last fetched" }))), el("tbody", null, rows));
}
function recentList(recent) {
    const ul = el("ul", { class: "recent" });
    for (const r of recent) {
        ul.appendChild(el("li", null, el("span", { class: "dot " + (r.status || "running") }), el("span", { class: "r-kind", text: r.kind }), el("span", { class: "r-key", text: r.key }), el("span", { class: "r-trigger", text: r.trigger }), el("span", { class: "r-when", text: fmtTime(r.started_at) })));
    }
    return ul;
}
function adminTable(scopes) {
    if (scopes.length === 0) {
        return el("div", { class: "empty", text: "No cache scopes recorded yet." });
    }
    const rows = scopes.map((s) => {
        const known = !!s.login && s.login !== "(unknown)";
        const loginCell = el("td", null, el("div", { class: "login-cell" }, known ? el("img", { class: "avatar", src: avatarFor(s.login), alt: "" }) : null, el("span", { class: known ? "" : "login unknown", text: s.login || "(unknown)" }), s.is_self ? el("span", { class: "badge you", text: "you" }) : null));
        return el("tr", null, loginCell, el("td", { class: "fingerprint", text: s.actor }), ...COUNT_FIELDS.map(([k]) => el("td", { class: "num", text: String(countOf(s.counts, k)) })), el("td", { class: "num", text: String(sumCounts(s.counts)) }), el("td", { text: s.last_seen ? fmtTime(s.last_seen) : "—" }), el("td", { class: "actions-cell" }, s.actor_id ? scopeActions(s) : null));
    });
    return el("table", { class: "scopes" }, el("thead", null, el("tr", null, el("th", { text: "Login" }), el("th", { text: "Scope" }), ...COUNT_FIELDS.map(([, label]) => el("th", { class: "num", text: label })), el("th", { class: "num", text: "Total" }), el("th", { text: "Last seen" }), el("th", { text: "Inspect" }))), el("tbody", null, rows));
}
// scopeActions renders the per-scope admin actions: browse the cached rows, or
// run a consistency check against GitHub. Both open a modal.
function scopeActions(s) {
    return el("div", { class: "scope-actions" }, el("button", { class: "btn btn-sm", onclick: () => void openBrowse(s) }, "Browse"), el("button", { class: "btn btn-sm", onclick: () => void openCheck(s) }, "Check"));
}
// ---- webhook activity (admin only) ----
async function loadWebhooks() {
    const body = byId("scope-body");
    const sub = byId("scope-sub");
    body.innerHTML = "";
    body.appendChild(el("div", { class: "loading", text: "Loading webhook activity…" }));
    let data;
    try {
        data = await api("/api/webhooks");
    }
    catch (e) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Could not load webhook activity: " + e.message }));
        return;
    }
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
    ["applied", "data written to the cache"],
    ["skipped", "no cached scope for the repo"],
    ["invalidated", "marked stale; refetched on next read"],
    ["ignored", "event not tracked"],
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
        return el("tr", null, el("td", null, el("span", { class: "disp " + disp, text: disp })), el("td", { class: "wh-event", text: evt }), el("td", { class: "wh-repo", text: d.repo || "—" }), el("td", { class: "wh-detail", text: d.detail || "" }), el("td", { class: "num", text: String(d.actors) }), el("td", { class: "wh-delivery", title: d.delivery_id || "", text: shortID }), el("td", { class: "wh-when", text: fmtTime(d.received_at) }));
    });
    return el("table", { class: "webhooks" }, el("thead", null, el("tr", null, el("th", { text: "Result" }), el("th", { text: "Event" }), el("th", { text: "Repo" }), el("th", { text: "Detail" }), el("th", { class: "num", text: "Scopes" }), el("th", { text: "Delivery" }), el("th", { text: "Received" }))), el("tbody", null, rows));
}
// ---- admin: browse + consistency check (modal) ----
function scopeLabel(s) {
    return s.login && s.login !== "(unknown)" ? s.login : "scope " + s.actor;
}
// openModal creates a dismissable overlay and returns its (empty) body element.
function openModal(titleText) {
    const body = el("div", { class: "modal-body" });
    const closeBtn = el("button", { class: "btn btn-ghost modal-close", title: "Close" }, "✕");
    const card = el("div", { class: "modal-card" }, el("div", { class: "modal-head" }, el("div", { class: "modal-title", text: titleText }), closeBtn), body);
    const backdrop = el("div", { class: "modal-backdrop" }, card);
    const close = () => {
        backdrop.remove();
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
async function openBrowse(s) {
    const actorId = s.actor_id;
    if (!actorId)
        return;
    const { body } = openModal("Cache contents — " + scopeLabel(s));
    body.appendChild(el("div", { class: "loading", text: "Loading cached rows…" }));
    let data;
    try {
        data = await api("/api/cache/data?actor=" + encodeURIComponent(actorId));
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
    wrap.appendChild(el("div", { class: "detail-sub", text: "Full fingerprint " + d.actor_id + (d.login ? " · " + d.login : "") }));
    wrap.appendChild(totalsGrid(d.counts, 1));
    if (d.repos.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Repositories (" + d.repos.length + ")" }));
        wrap.appendChild(browseRepoTable(d.repos));
    }
    if (d.pull_requests.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Pull requests (" + d.pull_requests.length + ")" }));
        wrap.appendChild(browsePRTable(d.pull_requests));
    }
    const extras = [
        ["Orgs", d.orgs.length], ["Users", d.users.length],
        ["Comparisons", d.branch_comparisons.length], ["PR files", d.pr_files.length],
        ["Commit checks", d.commit_checks.length],
    ];
    const present = extras.filter(([, n]) => n > 0);
    if (present.length) {
        wrap.appendChild(el("div", { class: "section-label", text: "Also cached" }));
        wrap.appendChild(el("div", { class: "chips" }, ...present.map(([label, n]) => el("span", { class: "chip" }, label + " " + n))));
    }
    const raw = el("details", { class: "raw" }, el("summary", { text: "Raw JSON (everything cached for this scope)" }), jsonBlock(d, "Copy JSON"));
    wrap.appendChild(raw);
    return wrap;
}
function browseRepoTable(repos) {
    const rows = repos.map((r) => el("tr", null, el("td", null, el("a", { href: r.url, target: "_blank", rel: "noopener", text: r.name_with_owner })), el("td", { text: r.default_branch || "—" }), statusCell(r.default_branch_status), el("td", { text: r.is_archived ? "archived" : r.is_disabled ? "disabled" : "active" })));
    return el("table", { class: "kinds detail-table" }, el("thead", null, el("tr", null, el("th", { text: "Repo" }), el("th", { text: "Default branch" }), el("th", { text: "Branch status" }), el("th", { text: "Flags" }))), el("tbody", null, rows));
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
async function openCheck(s) {
    const actorId = s.actor_id;
    if (!actorId)
        return;
    const { body } = openModal("Consistency check — " + scopeLabel(s));
    body.appendChild(el("div", { class: "loading", text: "Fetching source of truth from GitHub and diffing… this can take a few seconds." }));
    let report;
    try {
        report = await api("/api/cache/check?actor=" + encodeURIComponent(actorId));
    }
    catch (e) {
        body.innerHTML = "";
        body.appendChild(el("div", { class: "error-banner", text: "Consistency check failed: " + e.message }));
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
    grid.appendChild(statCard(sm.repos_only_on_github, "Repos only on GH"));
    grid.appendChild(statCard(sm.prs_only_in_cache, "PRs only cached"));
    grid.appendChild(statCard(sm.prs_only_on_github, "PRs only on GH"));
    grid.appendChild(statCard(sm.field_mismatches, "Field mismatches"));
    wrap.appendChild(grid);
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
function discrepancyTable(items) {
    const rows = items.map((d) => {
        const where = d.kind === "pr" ? d.repo + " #" + (d.pr ?? "") : d.repo;
        return el("tr", null, el("td", null, el("span", { class: "disp issue-" + d.issue.replace(/_/g, "-"), text: d.issue.replace(/_/g, " ") })), el("td", { class: "wh-repo", text: where }), el("td", { class: "kind", text: d.field || "—" }), el("td", { class: "diff-cached", text: d.cached || "" }), el("td", { class: "diff-github", text: d.github || "" }), el("td", { class: "wh-detail", text: d.note || "" }));
    });
    return el("table", { class: "webhooks detail-table" }, el("thead", null, el("tr", null, el("th", { text: "Issue" }), el("th", { text: "Where" }), el("th", { text: "Field" }), el("th", { text: "Cached" }), el("th", { text: "GitHub" }), el("th", { text: "Note" }))), el("tbody", null, rows));
}
// ---- boot ----
async function boot() {
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
function demoApi(path) {
    const cfg = DEMO;
    const state = cfg.current ?? cfg.initial;
    const d = cfg.data[state] ?? {};
    if (path === "/api/me") {
        return Promise.resolve((d.me ?? { authenticated: false, login_configured: true, is_admin: false }));
    }
    if (path.startsWith("/api/cache/data")) {
        const b = d.browse?.[demoQuery(path, "actor")];
        return b ? Promise.resolve(b) : demoReject(404);
    }
    if (path.startsWith("/api/cache/check")) {
        const r = d.check?.[demoQuery(path, "actor")];
        return r ? Promise.resolve(r) : demoReject(503);
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
