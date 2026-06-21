// Shared DTO types for the dashboard front-end: the shapes returned by the
// /api/me, /api/cache, /api/webhooks, /api/cache/browse and /api/cache/check
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

export interface Counts {
    repos: number;
    pull_requests: number;
    orgs: number;
    users: number;
    commit_checks: number;
    pr_files: number;
    branch_comparisons: number;
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

export interface ScopeStats {
    actor: string;
    actor_id?: string;
    login: string;
    is_self: boolean;
    last_seen?: string;
    counts: Counts;
    total: number;
    kinds: KindFreshness[] | null;
    recent?: RecentRefresh[];
}

export interface CacheResponse {
    login: string;
    is_admin: boolean;
    scope: string;
    scope_count: number;
    totals: Counts;
    scopes: ScopeStats[] | null;
}

export interface WebhookDelivery {
    delivery_id: string;
    event_type: string;
    action: string;
    repo: string;
    received_at: string;
    disposition: string;
    detail: string;
    actors: number;
}

export interface WebhooksResponse {
    deliveries: WebhookDelivery[] | null;
}

// ---- request activity (cache hit/miss/passthrough) ----
export interface RequestEvent {
    actor: string;
    method: string;
    path: string;
    disposition: string;
    at: string;
}

export interface RequestsResponse {
    total: number;
    by_disposition: Record<string, number>;
    recent: RequestEvent[] | null;
}

// ---- admin cache browse ----
export interface BrowseRepo {
    owner: string;
    name: string;
    name_with_owner: string;
    url: string;
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
}

export interface BrowseOrg { login: string; url?: string; }
export interface BrowseUser { login: string; url?: string; avatar_url?: string; }
export interface BrowseComparison { owner: string; repo: string; base_ref: string; head_ref: string; ahead_by: number; behind_by: number; }
export interface BrowsePRFile { owner: string; repo: string; pr_number: number; path: string; additions: number; deletions: number; }
export interface BrowseCommitCheck { owner: string; repo: string; sha: string; context: string; state: string; }

export interface BrowseResponse {
    actor: string;
    actor_id: string;
    login?: string;
    counts: Counts;
    repos: BrowseRepo[];
    pull_requests: BrowsePR[];
    orgs: BrowseOrg[];
    users: BrowseUser[];
    branch_comparisons: BrowseComparison[];
    pr_files: BrowsePRFile[];
    commit_checks: BrowseCommitCheck[];
}

// ---- consistency check ----
export interface Discrepancy {
    kind: string;
    repo: string;
    pr?: number;
    issue: string;
    field?: string;
    cached?: string;
    github?: string;
    note?: string;
}

export interface OrgSkip { org: string; reason: string; }

export interface CheckSummary {
    orgs_checked: number;
    repos_cached: number;
    open_prs_cached: number;
    discrepancies: number;
    repos_only_in_cache: number;
    repos_only_on_github: number;
    prs_only_in_cache: number;
    prs_only_on_github: number;
    field_mismatches: number;
}

export interface ConsistencyReport {
    scope: string;
    scope_full: string;
    login?: string;
    fetched_as: string;
    generated_at: string;
    orgs_checked: string[];
    orgs_skipped?: OrgSkip[];
    summary: CheckSummary;
    discrepancies: Discrepancy[];
    notes?: string[];
}

export interface DemoStateData {
    me: Me;
    mine?: CacheResponse;
    all?: CacheResponse;
    webhooks?: WebhooksResponse;
    requests?: RequestsResponse;
    browse?: Record<string, BrowseResponse>; // keyed by actor_id
    check?: Record<string, ConsistencyReport>; // keyed by actor_id
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
