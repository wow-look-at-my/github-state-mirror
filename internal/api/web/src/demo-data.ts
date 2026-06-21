// Canned data for the standalone styling preview ONLY. Compiled by `npm run
// build` (tsc) to ../assets/demo-data.js. The production site never loads this
// file; the CI preview bundle injects a <script> tag for it so app.js renders
// these payloads instead of calling the backend. Edit this .ts, not the .js.

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
    error?: string;
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

interface WebhookDelivery {
    delivery_id: string;
    event_type: string;
    action: string;
    repo: string;
    received_at: string;
    disposition: string;
    detail: string;
    actors: number;
}

interface WebhooksResponse {
    deliveries: WebhookDelivery[] | null;
}

interface DemoStateData {
    me: Me;
    mine?: CacheResponse;
    all?: CacheResponse;
    webhooks?: WebhooksResponse;
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
        { kind: "compare", states: { fresh: 15, fetching: 2, error: 1 }, last_fetched: ago(30) },
    ],
    recent: [
        { kind: "compare", key: "wow-look-at-my/buildhost/main...feat", trigger: "lazy", started_at: ago(30), status: "running" },
        { kind: "pr_files", key: "wow-look-at-my/actions/318", trigger: "webhook", started_at: ago(210), status: "success" },
        { kind: "org_repos", key: "wow-look-at-my", trigger: "periodic", started_at: ago(810), status: "success" },
    ],
};
const pazerScopeCi: ScopeStats = {
    actor: "c1d2e3f4a5b6",
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

const pazerMine = [pazerScopeCli, pazerScopeCi];
const allScopes = [serviceScope, octocatScope, pazerScopeCli, pazerScopeCi, unknownScope].map((s) =>
    ({ ...s, is_self: s.login === "PazerOP" }));

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
            webhooks: demoWebhooks,
        },
    },
};

window.__GSM_DEMO__ = config;

export {};
