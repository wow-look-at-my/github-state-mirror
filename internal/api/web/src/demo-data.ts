// Canned data for the standalone styling preview ONLY. Compiled by `npm run
// build` (tsc) to ../assets/demo-data.js. The production site never loads this
// file; the CI preview bundle injects a <script> tag for it so app.js renders
// these payloads instead of calling the backend. Edit this .ts, not the .js.

import type {
    Counts, PrincipalStats, WebhooksResponse, BrowseResponse, GrantsResponse,
    ConsistencyReport, CheckProgressEvent, RequestsResponse, RateLimitResponse, DemoConfig,
} from "./types";

function ago(seconds: number): string {
    return new Date(Date.now() - seconds * 1000).toISOString();
}

function inFuture(seconds: number): string {
    return new Date(Date.now() + seconds * 1000).toISOString();
}

// The one global truth store's row counts (same numbers on every view).
const globalTotals: Counts = {
    repos: 55, pull_requests: 119, commit_checks: 471, contents: 38, git_commits: 204, grants: 61,
};

// --- octocat: a regular signed-in user (one principal) ---
const octocatPrincipal: PrincipalStats = {
    principal: "user:583231",
    principal_id: "user:583231",
    login: "octocat",
    is_self: true,
    last_seen: ago(120),
    live_grants: 9,
    kinds: [
        { kind: "org_repos", states: { fresh: 1, stale: 1 }, last_fetched: ago(900) },
    ],
    recent: [
        { kind: "org_repos", key: "octo-org", trigger: "lazy", started_at: ago(900), status: "success" },
        { kind: "org_repos", key: "octo-corp", trigger: "periodic", started_at: ago(3600), status: "success" },
        { kind: "org_repos", key: "octo-org", trigger: "lazy", started_at: ago(5400), status: "error", error: "github api POST /graphql: 502 Bad Gateway" },
    ],
};

// --- PazerOP (admin) ---
const pazerPrincipal: PrincipalStats = {
    principal: "user:9219827",
    principal_id: "user:9219827",
    login: "PazerOP",
    is_self: true,
    last_seen: ago(60),
    live_grants: 41,
    kinds: [
        { kind: "org_repos", states: { fresh: 4, stale: 1 }, last_fetched: ago(800) },
    ],
    recent: [
        { kind: "org_repos", key: "wow-look-at-my", trigger: "periodic", started_at: ago(810), status: "success" },
        { kind: "org_repos", key: "PazerOP", trigger: "lazy", started_at: ago(2400), status: "success" },
    ],
};

// --- other principals only the admin sees in the "Principals" view ---
const prMinderPrincipal: PrincipalStats = {
    principal: "app:3433933",
    principal_id: "app:3433933",
    login: "pr-minder",
    is_self: false,
    last_seen: ago(30),
    live_grants: 55,
    kinds: [
        { kind: "org_repos", states: { fresh: 6 }, last_fetched: ago(240) },
    ],
};
const refresherPrincipal: PrincipalStats = {
    principal: "app-installation:481",
    principal_id: "app-installation:481",
    login: "gsm-bot",
    is_self: false,
    last_seen: ago(21600),
    live_grants: 60,
    kinds: [
        { kind: "org_repos", states: { fresh: 6 }, last_fetched: ago(21600) },
    ],
};
const unknownPrincipal: PrincipalStats = {
    principal: "token:00ff11ee22dd",
    principal_id: "token:00ff11ee22dd33cc44bb55aa66990088",
    login: "(unknown)",
    is_self: false,
    live_grants: 0,
    kinds: [],
};

// --- webhook delivery log (admin "Webhooks" tab) ---
// Under the global model every stateful delivery applies (or invalidates on a
// structural change) — there is no "skipped": truth is maintained for repos
// nobody has fetched yet too.
const demoWebhooks: WebhooksResponse = {
    deliveries: [
        { delivery_id: "ddfad8a0-6ce9-11f1-9454-861fa0b5e50d", event_type: "pull_request", action: "edited", repo: "wow-look-at-my/buildhost", received_at: ago(5), disposition: "applied", detail: "upserted PR #318" },
        { delivery_id: "dbf2cd6a-6ce9-11f1-94a4-20dae95361b4", event_type: "status", action: "", repo: "wow-look-at-my/buildhost", received_at: ago(9), disposition: "applied", detail: "status:ci/build=SUCCESS, rollup=SUCCESS" },
        { delivery_id: "db19f030-6ce9-11f1-9657-775bf60e6774", event_type: "check_run", action: "completed", repo: "wow-look-at-my/buildhost", received_at: ago(10), disposition: "applied", detail: "check_run:test=SUCCESS, rollup=SUCCESS" },
        { delivery_id: "db1f7e10-6ce9-11f1-830a-4240fdd66fd2", event_type: "workflow_job", action: "queued", repo: "wow-look-at-my/buildhost", received_at: ago(10), disposition: "ignored", detail: "action queued not tracked" },
        { delivery_id: "d8388ac0-6ce9-11f1-8f30-7678518bf7a2", event_type: "pull_request", action: "labeled", repo: "octo-org/api", received_at: ago(11), disposition: "applied", detail: "upserted PR #12 (repo absorbed from payload)" },
        { delivery_id: "d6f2c450-6ce9-11f1-8f8a-0023eea7e213", event_type: "pull_request", action: "opened", repo: "wow-look-at-my/actions", received_at: ago(18), disposition: "applied", detail: "upserted PR #92" },
        { delivery_id: "d607e048-6ce9-11f1-9529-df37c1489e44", event_type: "repository", action: "renamed", repo: "wow-look-at-my/old-name", received_at: ago(40), disposition: "applied", detail: "renamed to wow-look-at-my/new-name" },
        { delivery_id: "d4e4e508-6ce9-11f1-9233-8efd5d462518", event_type: "push", action: "", repo: "wow-look-at-my/buildhost", received_at: ago(62), disposition: "applied", detail: "updated pushed_at; un-resolved mergeable on branch master" },
    ],
};

// --- request activity log (admin "Requests" tab) ---
// The route-shape groups sum exactly to by_disposition (hit 1503, miss 71,
// passthrough 226, write 38, error 4 -> total 1842), sorted by total desc like
// the backend, so the preview's share percentages are coherent.
const demoRequests: RequestsResponse = {
    total: 1842,
    by_disposition: { hit: 1503, miss: 71, passthrough: 226, write: 38, error: 4 },
    // Renders as "DB 1.4 GB (+125.8 MB WAL)" on the tab's summary line.
    db_size_bytes: 1437204480,
    db_wal_size_bytes: 125829120,
    groups: [
        { key: "POST /graphql", method: "POST", route: "/graphql", total: 1146, hit: 1103, miss: 41, passthrough: 0, write: 0, error: 2, sample: "/graphql", last_seen: ago(2) },
        { key: "GET /repos/{owner}/{repo}/pulls/{number}", method: "GET", route: "/repos/{owner}/{repo}/pulls/{number}", total: 248, hit: 236, miss: 12, passthrough: 0, write: 0, error: 0, sample: "/repos/wow-look-at-my/buildhost/pulls/318", last_seen: ago(3) },
        { key: "GET /repos/{owner}/{repo}/compare/{basehead}", method: "GET", route: "/repos/{owner}/{repo}/compare/{basehead}", total: 149, hit: 102, miss: 9, passthrough: 38, write: 0, error: 0, sample: "/repos/wow-look-at-my/buildhost/compare/main...release", last_seen: ago(4) },
        { key: "GET /repos/{owner}/{repo}/commits", method: "GET", route: "/repos/{owner}/{repo}/commits", total: 66, hit: 0, miss: 0, passthrough: 64, write: 0, error: 2, sample: "/repos/wow-look-at-my/actions/commits", last_seen: ago(31) },
        { key: "GET /search/issues", method: "GET", route: "/search/issues", total: 58, hit: 0, miss: 0, passthrough: 58, write: 0, error: 0, sample: "/search/issues", last_seen: ago(6) },
        { key: "GET /repos/{owner}/{repo}/commits/{ref}/status", method: "GET", route: "/repos/{owner}/{repo}/commits/{ref}/status", total: 49, hit: 44, miss: 5, passthrough: 0, write: 0, error: 0, sample: "/repos/wow-look-at-my/buildhost/commits/master/status", last_seen: ago(12) },
        { key: "GET /rate_limit", method: "GET", route: "/rate_limit", total: 41, hit: 0, miss: 0, passthrough: 41, write: 0, error: 0, sample: "/rate_limit", last_seen: ago(14) },
        { key: "GET /repos/{owner}/{repo}/git/refs/heads/…", method: "GET", route: "/repos/{owner}/{repo}/git/refs/heads/…", total: 25, hit: 0, miss: 0, passthrough: 25, write: 0, error: 0, sample: "/repos/wow-look-at-my/buildhost/git/refs/heads/oci-cache", last_seen: ago(65) },
        { key: "GET /repos/{owner}/{repo}/commits/{ref}/check-runs", method: "GET", route: "/repos/{owner}/{repo}/commits/{ref}/check-runs", total: 22, hit: 18, miss: 4, passthrough: 0, write: 0, error: 0, sample: "/repos/wow-look-at-my/buildhost/commits/9f3c1a2b9f3c1a2b9f3c1a2b9f3c1a2b9f3c1a2b/check-runs", last_seen: ago(13) },
        { key: "PATCH /repos/{owner}/{repo}/pulls/{number}", method: "PATCH", route: "/repos/{owner}/{repo}/pulls/{number}", total: 21, hit: 0, miss: 0, passthrough: 0, write: 21, error: 0, sample: "/repos/wow-look-at-my/actions/pulls/92", last_seen: ago(20) },
        { key: "PUT /repos/{owner}/{repo}/pulls/{number}/update-branch", method: "PUT", route: "/repos/{owner}/{repo}/pulls/{number}/update-branch", total: 17, hit: 0, miss: 0, passthrough: 0, write: 17, error: 0, sample: "/repos/wow-look-at-my/buildhost/pulls/318/update-branch", last_seen: ago(24) },
    ],
    recent: [
        { actor: "app:3433933", method: "POST", path: "/graphql", disposition: "hit", at: ago(2) },
        { actor: "app:3433933", method: "GET", path: "/repos/wow-look-at-my/buildhost/pulls/318", disposition: "hit", at: ago(3) },
        { actor: "app:3433933", method: "GET", path: "/repos/wow-look-at-my/buildhost/compare/main...release", disposition: "passthrough", status: 200, at: ago(4) },
        { actor: "app:3433933", method: "GET", path: "/search/issues", disposition: "passthrough", status: 422, at: ago(6) },
        { actor: "app:3433933", method: "POST", path: "/graphql", disposition: "miss", at: ago(9) },
        { actor: "user:583231", method: "GET", path: "/rate_limit", disposition: "passthrough", status: 200, at: ago(14) },
        { actor: "app:3433933", method: "PATCH", path: "/repos/wow-look-at-my/actions/pulls/92", disposition: "write", status: 200, at: ago(20) },
        { actor: "app:3433933", method: "PUT", path: "/repos/wow-look-at-my/buildhost/pulls/318/update-branch", disposition: "write", status: 202, at: ago(24) },
    ],
};

// --- GitHub rate limit (admin "Rate limit" tab): the App's live poll plus
// --- passively observed X-RateLimit-* readings per (identity, resource) ---
const resetIn = (secs: number): number => Math.floor(Date.now() / 1000) + secs;
const demoRateLimit: RateLimitResponse = {
    live: [
        {
            // A realistic full bucket set — GitHub's /rate_limit returns ~15
            // resources for an App — including the long names that used to
            // break the tile layout, plus buckets in the warn (≥70% used,
            // yellow) and critical (≥90% used, red) bands so the preview
            // demonstrates every meter state.
            installation: "wow-look-at-my", account_type: "Organization",
            resources: {
                core: { limit: 15000, remaining: 14231, used: 769, reset: resetIn(2520) },
                graphql: { limit: 5000, remaining: 392, used: 4608, reset: resetIn(540) },
                search: { limit: 30, remaining: 30, used: 0, reset: resetIn(60) },
                actions_runner_registration: { limit: 10000, remaining: 10000, used: 0, reset: resetIn(3600) },
                audit_log: { limit: 1750, remaining: 1737, used: 13, reset: resetIn(2942) },
                audit_log_streaming: { limit: 15, remaining: 15, used: 0, reset: resetIn(3600) },
                code_scanning_autofix: { limit: 10, remaining: 1, used: 9, reset: resetIn(48) },
                code_search: { limit: 10, remaining: 2, used: 8, reset: resetIn(37) },
                copilot_usage_records: { limit: 1750, remaining: 1750, used: 0, reset: resetIn(3600) },
                dependency_sbom: { limit: 100, remaining: 96, used: 4, reset: resetIn(1210) },
                dependency_snapshots: { limit: 100, remaining: 100, used: 0, reset: resetIn(60) },
                integration_manifest: { limit: 5000, remaining: 5000, used: 0, reset: resetIn(3600) },
                scim: { limit: 15000, remaining: 15000, used: 0, reset: resetIn(3600) },
                source_import: { limit: 100, remaining: 100, used: 0, reset: resetIn(60) },
            },
        },
        {
            installation: "PazerOP", account_type: "User",
            resources: {
                core: { limit: 5000, remaining: 4980, used: 20, reset: resetIn(3300) },
                graphql: { limit: 5000, remaining: 5000, used: 0, reset: resetIn(3300) },
            },
        },
    ],
    observed: [
        // Sorted by identity then resource, matching the backend's snapshot.
        { identity: "app-installation:481", resource: "core", limit: 15000, remaining: 14980, used: 20, reset: resetIn(3300), observed_at: ago(300) },
        // Zero-usage reading (nothing consumed this window, e.g. only 304s) —
        // hidden by the client-side used > 0 filter, so the preview exercises it.
        { identity: "app-installation:481", resource: "graphql", limit: 12500, remaining: 12500, used: 0, reset: resetIn(3300), observed_at: ago(300) },
        { identity: "app-jwt", resource: "core", limit: 15000, remaining: 14999, used: 1, reset: resetIn(3400), observed_at: ago(75) },
        { identity: "app:3433933", resource: "core", limit: 15000, remaining: 13890, used: 1110, reset: resetIn(2520), observed_at: ago(12) },
        { identity: "app:3433933", resource: "graphql", limit: 5000, remaining: 4990, used: 10, reset: resetIn(540), observed_at: ago(95) },
        { identity: "token:00ff11ee22dd", resource: "core", limit: 60, remaining: 5, used: 55, reset: resetIn(900), observed_at: ago(600) },
        { identity: "user:583231", resource: "core", limit: 5000, remaining: 4300, used: 700, reset: resetIn(1800), observed_at: ago(30) },
    ],
};

const allPrincipals = [prMinderPrincipal, octocatPrincipal, pazerPrincipal, refresherPrincipal, unknownPrincipal].map((s) =>
    ({ ...s, is_self: s.login === "PazerOP" }));

// --- admin: browse (global truth), grants (per principal), check (global) ---
const demoBrowse: BrowseResponse = {
    counts: globalTotals,
    repos: [
        { owner: "wow-look-at-my", name: "buildhost", name_with_owner: "wow-look-at-my/buildhost", url: "https://github.com/wow-look-at-my/buildhost", visibility: "public", is_disabled: false, is_archived: false, pushed_at: ago(300), default_branch: "master", default_branch_status: "SUCCESS" },
        { owner: "wow-look-at-my", name: "actions", name_with_owner: "wow-look-at-my/actions", url: "https://github.com/wow-look-at-my/actions", visibility: "private", is_disabled: false, is_archived: false, pushed_at: ago(5400), default_branch: "master", default_branch_status: "FAILURE" },
        { owner: "octo-org", name: "api", name_with_owner: "octo-org/api", url: "https://github.com/octo-org/api", visibility: "", is_disabled: false, is_archived: false, pushed_at: ago(11) },
    ],
    pull_requests: [
        { owner: "wow-look-at-my", repo: "buildhost", number: 318, title: "Add OCI manifest cache", url: "https://github.com/wow-look-at-my/buildhost/pull/318", state: "OPEN", is_draft: false, author_login: "PazerOP", base_ref: "master", head_ref: "oci-cache", head_sha: "9f3c1a2", additions: 220, deletions: 14, mergeable: "MERGEABLE", review_requests: 1, last_commit_status: "SUCCESS", labels: ["enhancement"], created_at: ago(86400), updated_at: ago(5), touched_at: ago(5), rest_complete: true },
        { owner: "wow-look-at-my", repo: "actions", number: 92, title: "Fix typescript action newline", url: "https://github.com/wow-look-at-my/actions/pull/92", state: "OPEN", is_draft: true, author_login: "dependabot", base_ref: "master", head_ref: "fix-newline", head_sha: "1b2c3d4", additions: 4, deletions: 2, mergeable: "UNKNOWN", review_requests: 0, last_commit_status: "PENDING", labels: ["bug", "ci"], created_at: ago(18000), updated_at: ago(18), touched_at: ago(18), rest_complete: false },
    ],
    commit_checks: [{ owner: "wow-look-at-my", repo: "buildhost", sha: "9f3c1a2", context: "ci/build", state: "SUCCESS" }],
};

function demoGrants(p: PrincipalStats): GrantsResponse {
    return {
        principal: p.principal,
        principal_id: p.principal_id,
        login: p.login === "(unknown)" ? undefined : p.login,
        grants: p.live_grants === 0 ? [] : [
            { owner: "wow-look-at-my", repo: "buildhost", source: "list_sync", granted_at: ago(810), expires_at: inFuture(85590) },
            { owner: "wow-look-at-my", repo: "actions", source: "list_sync", granted_at: ago(810), expires_at: inFuture(85590) },
            { owner: "octo-org", repo: "api", source: "probe", granted_at: ago(3600), expires_at: inFuture(82800) },
        ],
    };
}

const demoCheck: ConsistencyReport = {
    fetched_as: "github-app",
    generated_at: ago(2),
    orgs_checked: ["wow-look-at-my"],
    orgs_skipped: [{ org: "octo-org", reason: "no GitHub App installation for this owner (app not installed, or no access)" }],
    truth_freshness: {
        "wow-look-at-my": { state: "fresh", last_fetched_at: ago(810), principal: "app:3433933" },
        "octo-org": { state: "error", last_fetched_at: ago(5400), principal: "user:583231", error: "github api POST /graphql: 502 Bad Gateway" },
    },
    summary: { orgs_checked: 1, repos_cached: 55, open_prs_cached: 119, discrepancies: 6, repos_only_in_cache: 2, repos_only_on_github: 1, repos_only_on_github_private: 1, repos_only_in_cache_archived: 1, prs_only_in_cache: 1, prs_only_on_github: 0, field_mismatches: 2, visibility_leaks: 1 },
    discrepancies: [
        { kind: "pr", repo: "wow-look-at-my/actions", pr: 92, issue: "field_mismatch", field: "last_commit_status", cached: "PENDING", github: "SUCCESS", note: "webhook-aggregated rollup diverged from GitHub", fix: "apply mode deletes the contradicted commit_checks rows and sets GitHub's rollup, so the next webhook cannot re-poison it" },
        { kind: "pr", repo: "wow-look-at-my/buildhost", pr: 318, issue: "field_mismatch", field: "label:enhancement", cached: "(absent)", github: "a2eeef", fix: "apply mode overwrites it via the truth sync" },
        { kind: "pr", repo: "wow-look-at-my/buildhost", pr: 301, issue: "only_in_cache", title: "Retry OCI blob mounts", updated_at: ago(200000), touched_at: ago(190000), served_now: true, note: "cached as open but not in GitHub's open PRs (likely closed/merged; a webhook was missed)", fix: "apply mode deletes the stale open row; a mirrored read of the PR also self-heals it" },
        { kind: "repo", repo: "wow-look-at-my/old-name", issue: "only_in_cache", archived: true, note: "archived on GitHub; archived repos are excluded from the org data fetch -- expected, not drift", fix: "none needed: archived repos stay cached by design" },
        { kind: "repo", repo: "wow-look-at-my/lab", issue: "visibility_leak", field: "visibility", cached: "public", github: "private", note: "SECURITY: cached public but private on GitHub -- the reveal fast path is serving this repo's cached state to any authenticated caller", fix: "apply mode sets visibility from GitHub's answer" },
        { kind: "repo", repo: "wow-look-at-my/secret-lab", issue: "only_on_github", visibility: "private", note: "private repo not yet absorbed: no webhook and no principal's sync has referenced it; expected under lazy truth", fix: "apply mode absorbs it (POST /api/cache/check?apply=true)" },
    ],
    notes: [
        "Source of truth was fetched as the mirror's GitHub App (repositoryOwner query, so User-account installations are checked too). Owners the app is not installed on are skipped (listed under orgs_skipped), not reported as missing.",
        "Only OPEN pull requests are compared (the cache only retains open PRs).",
        "The mergeable field is not compared: the cache deliberately un-resolves it on pushes and the GraphQL/REST readings race GitHub's recomputation.",
    ],
};

// The Reconcile (apply=true) answer: same report shape plus the corrections
// tally, so the preview exercises the applied grid.
const demoCheckApplied: ConsistencyReport = {
    ...demoCheck,
    applied: {
        repos_absorbed: 1, prs_absorbed: 2, prs_deleted: 1, visibility_set: 3,
        statuses_corrected: 1, check_rows_deleted: 4, default_branch_status_set: 2, auto_merge_set: 1,
    },
    notes: [
        ...(demoCheck.notes ?? []),
        "Apply mode: corrections were written AFTER the diff was taken -- discrepancies show the PRE-apply state, and 'applied' tallies the corrections.",
    ],
};

// A short canned progress sequence (the real endpoint streams these as NDJSON
// lines) so the preview's check/Reconcile modal exercises the live progress
// bar before the canned report renders. Replayed on a timer by the app's demo
// stream shim.
const demoCheckProgress: CheckProgressEvent[] = [
    { phase: "start", owners: 2 },
    { phase: "owner", owner: "wow-look-at-my", index: 1, total: 2 },
    { phase: "fetch", owner: "wow-look-at-my", repos_fetched: 5, repos_total: 55 },
    { phase: "fetch", owner: "wow-look-at-my", repos_fetched: 25, repos_total: 55 },
    { phase: "fetch", owner: "wow-look-at-my", repos_fetched: 55, repos_total: 55 },
    { phase: "visibility", owner: "wow-look-at-my" },
    { phase: "diffed", owner: "wow-look-at-my", discrepancies: 6 },
    { phase: "owner", owner: "octo-org", index: 2, total: 2 },
    { phase: "skip", owner: "octo-org", reason: "no GitHub App installation for this owner (app not installed, or no access)" },
    { phase: "done" },
];

const adminGrants: Record<string, GrantsResponse> = {};
for (const s of allPrincipals) {
    if (!s.principal_id) continue;
    adminGrants[s.principal_id] = demoGrants(s);
}

const config: DemoConfig = {
    initial: "admin",
    data: {
        "logged-out": {
            me: { authenticated: false, login_configured: true, is_admin: false },
        },
        "user": {
            me: { authenticated: true, login_configured: true, login: "octocat", is_admin: false },
            mine: {
                login: "octocat", is_admin: false, scope: "mine", principal_count: 1,
                totals: globalTotals, principals: [octocatPrincipal],
            },
        },
        "admin": {
            me: { authenticated: true, login_configured: true, login: "PazerOP", is_admin: true },
            mine: {
                login: "PazerOP", is_admin: true, scope: "mine", principal_count: 1,
                totals: globalTotals, principals: [pazerPrincipal],
            },
            all: {
                login: "PazerOP", is_admin: true, scope: "all", principal_count: allPrincipals.length,
                totals: globalTotals, principals: allPrincipals,
            },
            requests: demoRequests,
            webhooks: demoWebhooks,
            ratelimit: demoRateLimit,
            browse: demoBrowse,
            grants: adminGrants,
            check: demoCheck,
            checkApplied: demoCheckApplied,
            checkProgress: demoCheckProgress,
        },
    },
};

window.__GSM_DEMO__ = config;

export {};
