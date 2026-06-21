// Canned data for the standalone styling preview ONLY. Compiled by `npm run
// build` (tsc) to ../assets/demo-data.js. The production site never loads this
// file; the CI preview bundle injects a <script> tag for it so app.js renders
// these payloads instead of calling the backend. Edit this .ts, not the .js.

import type { Counts, ScopeStats, WebhooksResponse, BrowseResponse, ConsistencyReport, RequestsResponse, RateLimitResponse, DemoConfig } from "./types";

function ago(seconds: number): string {
    return new Date(Date.now() - seconds * 1000).toISOString();
}

function counts(c: Partial<Counts>): Counts {
    return {
        repos: 0, pull_requests: 0, orgs: 0, users: 0,
        commit_checks: 0, pr_files: 0, branch_comparisons: 0, ...c,
    };
}

function total(c: Counts): number {
    return c.repos + c.pull_requests + c.orgs + c.users + c.commit_checks + c.pr_files + c.branch_comparisons;
}

// --- octocat: a regular signed-in user with a single token scope ---
const octocatCounts = counts({ repos: 14, pull_requests: 23, orgs: 2, users: 1, commit_checks: 61, pr_files: 188, branch_comparisons: 4 });
const octocatScope: ScopeStats = {
    actor: "9f86d081884c",
    actor_id: "9f86d081884c4d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f0fp01",
    login: "octocat",
    is_self: true,
    last_seen: ago(120),
    counts: octocatCounts,
    total: total(octocatCounts),
    kinds: [
        { kind: "user", states: { fresh: 1 }, last_fetched: ago(120) },
        { kind: "user_orgs", states: { fresh: 1 }, last_fetched: ago(125) },
        { kind: "org_repos", states: { fresh: 1, stale: 1 }, last_fetched: ago(900) },
        { kind: "pr_files", states: { fresh: 18, stale: 3, error: 1 }, last_fetched: ago(300) },
        { kind: "compare", states: { fresh: 3, fetching: 1 }, last_fetched: ago(45) },
    ],
    recent: [
        { kind: "pr_files", key: "octo-org/api/142", trigger: "lazy", started_at: ago(300), status: "success" },
        { kind: "compare", key: "octo-org/api/main...release", trigger: "webhook", started_at: ago(640), status: "success" },
        { kind: "org_repos", key: "octo-org", trigger: "periodic", started_at: ago(3600), status: "success" },
        { kind: "pr_files", key: "octo-org/web/77", trigger: "lazy", started_at: ago(5400), status: "error", error: "github api GET /repos/octo-org/web/pulls/77/files: 404 Not Found" },
    ],
};

// --- PazerOP (admin): two of their own token scopes ---
const pazerCli = counts({ repos: 41, pull_requests: 96, orgs: 5, users: 1, commit_checks: 220, pr_files: 742, branch_comparisons: 18 });
const pazerCi = counts({ repos: 12, pull_requests: 8, orgs: 1, users: 1, commit_checks: 33, pr_files: 0, branch_comparisons: 2 });
const pazerScopeCli: ScopeStats = {
    actor: "a3f5c9d20b71",
    actor_id: "a3f5c9d20b71e6f4c0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5ffp02",
    login: "PazerOP",
    is_self: true,
    last_seen: ago(60),
    counts: pazerCli,
    total: total(pazerCli),
    kinds: [
        { kind: "user", states: { fresh: 1 }, last_fetched: ago(60) },
        { kind: "user_orgs", states: { fresh: 1 }, last_fetched: ago(65) },
        { kind: "org_repos", states: { fresh: 4, stale: 1 }, last_fetched: ago(800) },
        { kind: "pr_files", states: { fresh: 70, stale: 12 }, last_fetched: ago(200) },
        { kind: "compare", states: { fresh: 15, fetching: 2, error: 1 }, last_fetched: ago(30), error: "github api GET /repos/wow-look-at-my/buildhost/compare/main...feat: 404 Not Found", error_key: "wow-look-at-my/buildhost/main...feat" },
    ],
    recent: [
        { kind: "compare", key: "wow-look-at-my/buildhost/main...feat", trigger: "lazy", started_at: ago(30), status: "running" },
        { kind: "pr_files", key: "wow-look-at-my/actions/318", trigger: "webhook", started_at: ago(210), status: "success" },
        { kind: "org_repos", key: "wow-look-at-my", trigger: "periodic", started_at: ago(810), status: "success" },
    ],
};
const pazerScopeCi: ScopeStats = {
    actor: "c1d2e3f4a5b6",
    actor_id: "c1d2e3f4a5b6f7081920a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6fp03",
    login: "PazerOP",
    is_self: true,
    last_seen: ago(7200),
    counts: pazerCi,
    total: total(pazerCi),
    kinds: [
        { kind: "org_repos", states: { fresh: 1 }, last_fetched: ago(7200) },
        { kind: "pr_files", states: { stale: 4 }, last_fetched: ago(9000) },
    ],
    recent: [
        { kind: "org_repos", key: "PazerOP", trigger: "periodic", started_at: ago(7200), status: "success" },
    ],
};

// --- other scopes only the admin sees in the "all" view ---
const serviceCounts = counts({ repos: 60, pull_requests: 140, orgs: 6, users: 1, commit_checks: 410, pr_files: 0, branch_comparisons: 0 });
const serviceScope: ScopeStats = {
    actor: "deadbeef0001",
    actor_id: "app-installation:481",
    login: "gsm-bot",
    is_self: false,
    last_seen: ago(21600),
    counts: serviceCounts,
    total: total(serviceCounts),
    kinds: [
        { kind: "org_repos", states: { fresh: 6 }, last_fetched: ago(21600) },
        { kind: "user_orgs", states: { fresh: 1 }, last_fetched: ago(21600) },
    ],
};
const unknownCounts = counts({ repos: 3, pull_requests: 5, orgs: 0, users: 1, commit_checks: 9, pr_files: 22, branch_comparisons: 1 });
const unknownScope: ScopeStats = {
    actor: "00ff11ee22dd",
    actor_id: "00ff11ee22dd33cc44bb55aa66990088112233445566778899aabbccddeefp05",
    login: "(unknown)",
    is_self: false,
    counts: unknownCounts,
    total: total(unknownCounts),
    kinds: [
        { kind: "pr_files", states: { stale: 2 }, last_fetched: ago(172800) },
    ],
};

function sumScopes(list: ScopeStats[]): Counts {
    return list.reduce<Counts>((acc, s) => counts({
        repos: acc.repos + s.counts.repos,
        pull_requests: acc.pull_requests + s.counts.pull_requests,
        orgs: acc.orgs + s.counts.orgs,
        users: acc.users + s.counts.users,
        commit_checks: acc.commit_checks + s.counts.commit_checks,
        pr_files: acc.pr_files + s.counts.pr_files,
        branch_comparisons: acc.branch_comparisons + s.counts.branch_comparisons,
    }), counts({}));
}

// --- webhook delivery log (admin "Webhooks" tab) ---
const demoWebhooks: WebhooksResponse = {
    deliveries: [
        { delivery_id: "ddfad8a0-6ce9-11f1-9454-861fa0b5e50d", event_type: "pull_request", action: "edited", repo: "wow-look-at-my/buildhost", received_at: ago(5), disposition: "applied", detail: "upserted PR #318", actors: 2 },
        { delivery_id: "dbf2cd6a-6ce9-11f1-94a4-20dae95361b4", event_type: "status", action: "", repo: "wow-look-at-my/buildhost", received_at: ago(9), disposition: "applied", detail: "status:ci/build=SUCCESS, rollup=SUCCESS", actors: 2 },
        { delivery_id: "db19f030-6ce9-11f1-9657-775bf60e6774", event_type: "check_run", action: "completed", repo: "wow-look-at-my/buildhost", received_at: ago(10), disposition: "applied", detail: "check_run:test=SUCCESS, rollup=SUCCESS", actors: 2 },
        { delivery_id: "db1f7e10-6ce9-11f1-830a-4240fdd66fd2", event_type: "workflow_job", action: "completed", repo: "wow-look-at-my/buildhost", received_at: ago(10), disposition: "ignored", detail: "event type not tracked", actors: 0 },
        { delivery_id: "d8388ac0-6ce9-11f1-8f30-7678518bf7a2", event_type: "pull_request", action: "labeled", repo: "octo-org/api", received_at: ago(11), disposition: "skipped", detail: "no cached scope for octo-org/api", actors: 0 },
        { delivery_id: "d6f2c450-6ce9-11f1-8f8a-0023eea7e213", event_type: "pull_request", action: "opened", repo: "wow-look-at-my/actions", received_at: ago(18), disposition: "applied", detail: "upserted PR #92", actors: 1 },
        { delivery_id: "d607e048-6ce9-11f1-9529-df37c1489e44", event_type: "repository", action: "renamed", repo: "wow-look-at-my/old-name", received_at: ago(40), disposition: "invalidated", detail: "structural change; marked org repos stale", actors: 0 },
        { delivery_id: "d4e4e508-6ce9-11f1-9233-8efd5d462518", event_type: "push", action: "", repo: "wow-look-at-my/buildhost", received_at: ago(62), disposition: "applied", detail: "updated pushed_at", actors: 2 },
    ],
};

// --- request activity log (admin "Requests" tab) ---
const demoRequests: RequestsResponse = {
    total: 1842,
    by_disposition: { hit: 1503, miss: 71, passthrough: 264, error: 4 },
    recent: [
        { actor: "app:3433933", method: "POST", path: "/graphql", disposition: "hit", at: ago(2) },
        { actor: "app:3433933", method: "GET", path: "/repos/wow-look-at-my/buildhost/pulls/318", disposition: "passthrough", status: 200, at: ago(3) },
        { actor: "app:3433933", method: "GET", path: "/repos/wow-look-at-my/buildhost/compare/main...release", disposition: "passthrough", status: 200, at: ago(4) },
        { actor: "app:3433933", method: "GET", path: "/search/issues", disposition: "passthrough", status: 422, at: ago(6) },
        { actor: "app:3433933", method: "POST", path: "/graphql", disposition: "miss", at: ago(9) },
        { actor: "token:9f86d0818", method: "GET", path: "/rate_limit", disposition: "passthrough", status: 200, at: ago(14) },
        { actor: "app:3433933", method: "PATCH", path: "/repos/wow-look-at-my/actions/pulls/92", disposition: "passthrough", status: 502, at: ago(20) },
    ],
};

// --- GitHub App rate limit (admin "Rate limit" tab) ---
const resetIn = (secs: number): number => Math.floor(Date.now() / 1000) + secs;
const demoRateLimit: RateLimitResponse = {
    installations: [
        {
            installation: "wow-look-at-my", account_type: "Organization",
            resources: {
                core: { limit: 15000, remaining: 14231, used: 769, reset: resetIn(2520) },
                graphql: { limit: 5000, remaining: 392, used: 4608, reset: resetIn(540) },
                search: { limit: 30, remaining: 30, used: 0, reset: resetIn(60) },
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
};

const pazerMine = [pazerScopeCli, pazerScopeCi];
const allScopes = [serviceScope, octocatScope, pazerScopeCli, pazerScopeCi, unknownScope].map((s) =>
    ({ ...s, is_self: s.login === "PazerOP" }));

// --- admin: browse + consistency check demo payloads (keyed by actor_id) ---
function demoOwner(login: string): string {
    return login === "octocat" ? "octo-org" : "wow-look-at-my";
}

function demoBrowse(s: ScopeStats): BrowseResponse {
    const owner = demoOwner(s.login);
    return {
        actor: s.actor,
        actor_id: s.actor_id ?? s.actor,
        login: s.login === "(unknown)" ? undefined : s.login,
        counts: s.counts,
        repos: [
            { owner, name: "buildhost", name_with_owner: owner + "/buildhost", url: "https://github.com/" + owner + "/buildhost", is_disabled: false, is_archived: false, pushed_at: ago(300), default_branch: "master", default_branch_status: "SUCCESS" },
            { owner, name: "actions", name_with_owner: owner + "/actions", url: "https://github.com/" + owner + "/actions", is_disabled: false, is_archived: false, pushed_at: ago(5400), default_branch: "master", default_branch_status: "FAILURE" },
        ],
        pull_requests: [
            { owner, repo: "buildhost", number: 318, title: "Add OCI manifest cache", url: "https://github.com/" + owner + "/buildhost/pull/318", state: "OPEN", is_draft: false, author_login: "PazerOP", base_ref: "master", head_ref: "oci-cache", head_sha: "9f3c1a2", additions: 220, deletions: 14, mergeable: "MERGEABLE", review_requests: 1, last_commit_status: "SUCCESS", labels: ["enhancement"], created_at: ago(86400), updated_at: ago(5) },
            { owner, repo: "actions", number: 92, title: "Fix typescript action newline", url: "https://github.com/" + owner + "/actions/pull/92", state: "OPEN", is_draft: true, author_login: "dependabot", base_ref: "master", head_ref: "fix-newline", head_sha: "1b2c3d4", additions: 4, deletions: 2, mergeable: "UNKNOWN", review_requests: 0, last_commit_status: "PENDING", labels: ["bug", "ci"], created_at: ago(18000), updated_at: ago(18) },
        ],
        orgs: [{ login: owner, url: "https://github.com/" + owner }],
        users: [{ login: s.login, url: "https://github.com/" + s.login }],
        branch_comparisons: [{ owner, repo: "buildhost", base_ref: "master", head_ref: "oci-cache", ahead_by: 3, behind_by: 0 }],
        pr_files: [{ owner, repo: "buildhost", pr_number: 318, path: "internal/oci/manifest.go", additions: 120, deletions: 4 }],
        commit_checks: [{ owner, repo: "buildhost", sha: "9f3c1a2", context: "ci/build", state: "SUCCESS" }],
    };
}

function demoCheck(s: ScopeStats): ConsistencyReport {
    const owner = demoOwner(s.login);
    return {
        scope: s.actor,
        scope_full: s.actor_id ?? s.actor,
        login: s.login === "(unknown)" ? undefined : s.login,
        fetched_as: "github-app",
        generated_at: ago(2),
        orgs_checked: [owner],
        orgs_skipped: [{ org: "octo-org", reason: "no GitHub App installation for this owner (app not installed, or no access)" }],
        summary: { orgs_checked: 1, repos_cached: s.counts.repos, open_prs_cached: s.counts.pull_requests, discrepancies: 4, repos_only_in_cache: 1, repos_only_on_github: 0, prs_only_in_cache: 1, prs_only_on_github: 0, field_mismatches: 2 },
        discrepancies: [
            { kind: "pr", repo: owner + "/actions", pr: 92, issue: "field_mismatch", field: "last_commit_status", cached: "PENDING", github: "SUCCESS", note: "webhook-aggregated rollup diverged from GitHub" },
            { kind: "pr", repo: owner + "/buildhost", pr: 318, issue: "field_mismatch", field: "label:enhancement", cached: "(absent)", github: "a2eeef" },
            { kind: "pr", repo: owner + "/buildhost", pr: 301, issue: "only_in_cache", note: "cached as open but not in GitHub's open PRs (likely closed/merged; a webhook was missed)" },
            { kind: "repo", repo: owner + "/old-name", issue: "only_in_cache", note: "cached but not among GitHub's non-archived repos (archived, deleted, renamed, or no longer visible)" },
        ],
        notes: [
            "Source of truth was fetched as the mirror's GitHub App, which may not see exactly what the token that populated this scope sees.",
            "Only OPEN pull requests are compared (the cache only retains open PRs).",
        ],
    };
}

const adminBrowse: Record<string, BrowseResponse> = {};
const adminCheck: Record<string, ConsistencyReport> = {};
for (const s of allScopes) {
    if (!s.actor_id) continue;
    adminBrowse[s.actor_id] = demoBrowse(s);
    adminCheck[s.actor_id] = demoCheck(s);
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
                login: "octocat", is_admin: false, scope: "mine", scope_count: 1,
                totals: octocatCounts, scopes: [octocatScope],
            },
        },
        "admin": {
            me: { authenticated: true, login_configured: true, login: "PazerOP", is_admin: true },
            mine: {
                login: "PazerOP", is_admin: true, scope: "mine", scope_count: pazerMine.length,
                totals: sumScopes(pazerMine), scopes: pazerMine,
            },
            all: {
                login: "PazerOP", is_admin: true, scope: "all", scope_count: allScopes.length,
                totals: sumScopes(allScopes), scopes: allScopes,
            },
            requests: demoRequests,
            webhooks: demoWebhooks,
            ratelimit: demoRateLimit,
            browse: adminBrowse,
            check: adminCheck,
        },
    },
};

window.__GSM_DEMO__ = config;

export {};
