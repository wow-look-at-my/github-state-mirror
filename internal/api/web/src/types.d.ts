// Shared DTO types for the dashboard front-end: the shapes returned by the
// /api/me, /api/cache, /api/webhooks, /api/cache/data and /api/cache/check
// endpoints, plus the demo-preview config. Single source of truth — both app.ts
// (the renderer) and demo-data.ts (the preview fixtures) import these, so the
// two can never drift. Imported with `import type`, so nothing here emits into
// the runtime bundles (this is a declaration file: tsc produces no .js for it).

export interface Me {
    authenticated: boolean;
    login_configured: boolean;
    login?: string;
    is_admin: boolean;
}

// Counts are the GLOBAL truth store's row counts — one cache, one truth.
export interface Counts {
    repos: number;
    pull_requests: number;
    commit_checks: number;
    contents: number;
    git_commits: number;
    grants: number;
}

export interface KindFreshness {
    kind: string;
    states: Record<string, number>;
    last_fetched?: string;
    error?: string;
    error_key?: string;
}

export interface RecentRefresh {
    kind: string;
    key: string;
    trigger: string;
    started_at: string;
    status: string;
    error?: string;
}

// PrincipalStats is one principal's reveal-layer standing: who they are, how
// many repos they hold live grants for, and how fresh their org syncs are.
export interface PrincipalStats {
    principal: string; // short (display)
    principal_id: string; // full key (for admin views)
    login: string;
    is_self: boolean;
    last_seen?: string;
    live_grants: number;
    kinds: KindFreshness[] | null;
    recent?: RecentRefresh[];
}

export interface CacheResponse {
    login: string;
    is_admin: boolean;
    scope: string;
    totals: Counts;
    principal_count: number;
    principals: PrincipalStats[] | null;
    truth?: KindFreshness[] | null;
}

export interface WebhookDelivery {
    delivery_id: string;
    event_type: string;
    action: string;
    repo: string;
    received_at: string;
    disposition: string;
    detail: string;
}

export interface WebhooksResponse {
    deliveries: WebhookDelivery[] | null;
}

// ---- request activity (cache hit/miss/passthrough/write) ----
export interface RequestEvent {
    actor: string;
    method: string;
    path: string;
    disposition: string;
    status?: number; // upstream HTTP status for a passthrough/write (0/absent otherwise)
    at: string;
}

export interface RequestsResponse {
    total: number;
    by_disposition: Record<string, number>;
    recent: RequestEvent[] | null;
}

// ---- GitHub App rate limit ----
export interface RateLimitResource {
    limit: number;
    remaining: number;
    used: number;
    reset: number; // Unix epoch seconds
}

export interface InstallationRateLimit {
    installation: string;
    account_type?: string;
    resources?: Record<string, RateLimitResource>;
    error?: string;
}

// One passively observed X-RateLimit-* reading: the latest headers seen on an
// upstream response for one (identity, resource) pair. In-memory server-side;
// resets on restart.
export interface ObservedRateLimit {
    identity: string;
    resource: string;
    limit: number;
    remaining: number;
    used: number;
    reset: number; // Unix epoch seconds
    observed_at: string; // RFC3339
}

export interface RateLimitResponse {
    // live: the GitHub App's per-installation GET /rate_limit poll. Empty when
    // no App is configured or the poll failed (see note).
    live: InstallationRateLimit[] | null;
    // observed: passively recorded X-RateLimit-* readings, sorted by identity
    // then resource.
    observed: ObservedRateLimit[] | null;
    // note explains an empty/failed live poll.
    note?: string;
}

// ---- admin cache browse (global truth rows) ----
export interface BrowseRepo {
    owner: string;
    name: string;
    name_with_owner: string;
    url: string;
    visibility?: string; // '' / absent = unknown (treated private)
    is_disabled: boolean;
    is_archived: boolean;
    pushed_at?: string;
    default_branch?: string;
    default_branch_status?: string;
}

export interface BrowsePR {
    owner: string;
    repo: string;
    number: number;
    title: string;
    url: string;
    state: string;
    is_draft: boolean;
    author_login?: string;
    base_ref?: string;
    head_ref?: string;
    head_sha?: string;
    additions: number;
    deletions: number;
    mergeable?: string;
    review_requests: number;
    last_commit_status?: string;
    labels?: string[];
    created_at?: string;
    updated_at?: string;
    touched_at?: string;
    rest_complete?: boolean;
}

export interface BrowseCommitCheck { owner: string; repo: string; sha: string; context: string; state: string; }

export interface BrowseResponse {
    counts: Counts;
    repos: BrowseRepo[];
    pull_requests: BrowsePR[];
    commit_checks: BrowseCommitCheck[];
}

// ---- admin grants view (/api/cache/data?principal=...) ----
export interface BrowseGrant {
    owner: string;
    repo: string;
    source: string; // list_sync | probe
    granted_at: string;
    expires_at: string;
}

export interface GrantsResponse {
    principal: string; // short (display)
    principal_id: string; // full key
    login?: string;
    grants: BrowseGrant[] | null;
}

// ---- consistency check (global truth vs GitHub) ----
export interface Discrepancy {
    kind: string;
    repo: string;
    pr?: number;
    // only_in_cache | only_on_github | field_mismatch | visibility_leak | visibility_unknown
    issue: string;
    field?: string;
    cached?: string;
    github?: string;
    visibility?: string; // "private"/"internal" on an only_on_github repo never absorbed
    archived?: boolean; // only_in_cache repo whose absence is explained by archival
    title?: string; // cached PR detail on pr only_in_cache entries
    updated_at?: string;
    touched_at?: string;
    served_now?: boolean; // a live pulls-list marker is serving the wrong list right now
    note?: string;
    fix?: string; // short per-class remediation hint
}

export interface OrgSkip { org: string; reason: string; }

export interface CheckSummary {
    orgs_checked: number;
    repos_cached: number;
    open_prs_cached: number;
    discrepancies: number;
    repos_only_in_cache: number;
    repos_only_on_github: number;
    repos_only_on_github_private: number;
    repos_only_in_cache_archived: number;
    prs_only_in_cache: number;
    prs_only_on_github: number;
    field_mismatches: number;
    visibility_leaks: number;
}

// AppliedSummary tallies apply-mode (Reconcile) corrections per action.
export interface AppliedSummary {
    repos_absorbed: number;
    prs_absorbed: number;
    prs_deleted: number;
    visibility_set: number;
    statuses_corrected: number;
    check_rows_deleted: number;
    default_branch_status_set: number;
    auto_merge_set: number;
}

// TruthFreshness is one owner's most-recent org sync marker (any principal's).
export interface TruthFreshness {
    state: string;
    last_fetched_at?: string;
    error?: string;
    principal?: string;
}

export interface ConsistencyReport {
    fetched_as: string;
    generated_at: string;
    orgs_checked: string[];
    orgs_skipped?: OrgSkip[];
    truth_freshness?: Record<string, TruthFreshness>;
    summary: CheckSummary;
    applied?: AppliedSummary; // present only on an apply (Reconcile) run
    discrepancies: Discrepancy[];
    notes?: string[];
}

export interface DemoStateData {
    me: Me;
    mine?: CacheResponse;
    all?: CacheResponse;
    webhooks?: WebhooksResponse;
    requests?: RequestsResponse;
    ratelimit?: RateLimitResponse;
    browse?: BrowseResponse; // global truth rows (one cache)
    grants?: Record<string, GrantsResponse>; // keyed by principal_id
    check?: ConsistencyReport; // global check (read-only)
    checkApplied?: ConsistencyReport; // the Reconcile (apply=true) answer
}

export interface DemoConfig {
    initial: string;
    current?: string;
    data: Record<string, DemoStateData>;
}

declare global {
    interface Window {
        __GSM_DEMO__?: DemoConfig;
    }
}
