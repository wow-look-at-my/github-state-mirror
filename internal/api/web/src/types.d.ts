// Shared DTO types for the dashboard front-end: the shapes returned by the
// /api/me, /api/cache, and /api/webhooks endpoints, plus the demo-preview
// config. Single source of truth — both app.ts (the renderer) and demo-data.ts
// (the preview fixtures) import these, so the two can never drift. These are
// imported with `import type`, so nothing here emits into the runtime bundles.

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

export interface DemoStateData {
    me: Me;
    mine?: CacheResponse;
    all?: CacheResponse;
    webhooks?: WebhooksResponse;
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
