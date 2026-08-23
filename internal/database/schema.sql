-- The DB is a cache with no migrations. Changing this file rebuilds it: db.go
-- hashes it with comments scrubbed and compares that against the hash recorded
-- in the file, so editing a table nukes and editing this comment does not.
-- Nothing here needs declaring, and nothing else nukes -- history and the
-- post-deploy reconcile step are in docs/schema-history.md.
--
-- DATA MODEL (since v9): ONE GLOBAL TRUTH STORE. GitHub state tables (repos,
-- pull_requests, pr_labels, commit_checks, and the per-route response-cache
-- tables: contents_cache, git_commits_cache, commits_list_cache,
-- compare_cache, commit_ci_cache, pulls_list_cache, pull_files_cache,
-- closed_pull_cache, branches_list_cache)
-- hold ONE row per resource -- no actor/scope dimension. Webhooks and fetches
-- by any principal all write the same truth. What a caller may READ is decided
-- at serve time by the reveal-by-permission layer: a repo's cached state is
-- revealed to a principal iff the repo is public in global truth, or the
-- principal holds a fresh access_grants row for it (earned from GitHub's own
-- answers to that principal's requests). See CLAUDE.md "security model".

-- ============================================================================
-- Freshness / Cache Metadata (generic, no GitHub knowledge)
-- ============================================================================

CREATE TABLE schema_version (
    fingerprint TEXT NOT NULL   -- sha256 of this file, comments scrubbed and
                                -- whitespace collapsed. A file whose value
                                -- differs from the running binary's is a file
                                -- describing other tables, and is replaced.
);

-- Freshness markers. Two kinds of rows share this table:
--   - actor = a principal ("user:<id>", "app:<id>", "app-installation:<id>",
--     or a token fingerprint): that principal's per-owner org-repos LIST-SYNC
--     marker (kind 'org_repos'). Freshness of the principal's GRANT SET for
--     the owner, not of the data itself -- truth is refreshed as a side effect
--     of any principal's sync.
--   - actor = 'global': freshness of a piece of GLOBAL truth that has a
--     dedicated fetch path (kind 'repo_pulls', key 'owner/repo'). Any
--     principal's fetch refreshes it for everyone.
CREATE TABLE cache_metadata (
    actor           TEXT NOT NULL DEFAULT '',
    resource_kind   TEXT NOT NULL,
    resource_key    TEXT NOT NULL,
    last_fetched_at TEXT,           -- RFC3339
    last_changed_at TEXT,           -- RFC3339
    etag            TEXT,
    expires_at      TEXT,           -- RFC3339
    fetch_state     TEXT NOT NULL DEFAULT 'unknown',
    error_message   TEXT,
    retry_after     TEXT,           -- RFC3339
    PRIMARY KEY (actor, resource_kind, resource_key)
);

CREATE TABLE cache_refresh_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    actor           TEXT NOT NULL DEFAULT '',
    resource_kind   TEXT NOT NULL,
    resource_key    TEXT NOT NULL,
    triggered_by    TEXT NOT NULL,
    started_at      TEXT NOT NULL,
    completed_at    TEXT,
    success         INTEGER,
    records_changed INTEGER,
    error_message   TEXT
);

-- ============================================================================
-- GitHub Data Tables (GLOBAL truth -- one row per resource)
-- ============================================================================

-- visibility is the reveal layer's fast path: 'public' rows are readable by any
-- authenticated principal without a grant. It is learned from webhook payloads
-- (repository.private / repository.visibility, flipped by publicized/privatized
-- events) and from REST fetch payloads that carry it; the identity-locked
-- GraphQL org-repos selection set does NOT carry it, so a repo seeded only by
-- that fetch stays '' (unknown) and is treated as PRIVATE (fail closed) until a
-- webhook or REST absorb reveals it.
CREATE TABLE repos (
    owner                 TEXT NOT NULL,
    name                  TEXT NOT NULL,
    name_with_owner       TEXT NOT NULL,
    url                   TEXT NOT NULL,
    is_disabled           INTEGER NOT NULL DEFAULT 0,
    is_archived           INTEGER NOT NULL DEFAULT 0,
    visibility            TEXT NOT NULL DEFAULT '',  -- '' unknown | public | private | internal
    pushed_at             TEXT,
    default_branch        TEXT,
    default_branch_status TEXT,
    owner_login           TEXT,
    owner_avatar          TEXT,
    owner_url             TEXT,
    PRIMARY KEY (owner, name)
);

-- pull_requests rows come from three writers: the GraphQL org-repos fetch
-- (the org sync, which selects only the identity-locked GraphQL field set),
-- pull_request/pull_request_review webhooks (ParsePRPayload, full REST-shaped
-- objects), and the cached REST /pulls routes (absorbed responses). The
-- REST-only columns (node_id .. merge_commit_sha) are NULL on GraphQL-sourced
-- rows; a row is "rest-complete" (rebuildable as a trimmed REST response)
-- only when node_id and base_ref_oid are set -- see ghdata.PRRestComplete.
--
-- touched_at guards reconciles against racing webhooks (an org/pulls fetch
-- only deletes an open-PR row absent from its snapshot when the row was not
-- touched after the fetch began; GraphQL/REST list reads are eventually
-- consistent, so a just-webhooked PR can be missing from a snapshot taken
-- moments later) AND backstops missed close deliveries: a row untouched for
-- longer than the staleness window is not served by the single-PR route.
CREATE TABLE pull_requests (
    owner                TEXT NOT NULL,
    repo                 TEXT NOT NULL,
    number               INTEGER NOT NULL,
    title                TEXT NOT NULL,
    url                  TEXT NOT NULL,
    is_draft             INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL,
    additions            INTEGER,
    deletions            INTEGER,
    mergeable            TEXT,
    mergeable_state      TEXT,   -- GitHub's mergeable_state (clean|behind|blocked|unstable|dirty|draft|
                                 -- has_hooks|unknown). SINGLE-PR responses only, like the diff stats.
                                 -- It answers what `mergeable` cannot -- WHY a mergeable PR still will not
                                 -- merge -- and `behind` is the only statement anywhere that a strict
                                 -- up-to-date rule is the thing blocking it. Un-resolved with `mergeable`
                                 -- by every branch/tip move AND, unlike it, by CI events: unstable/blocked
                                 -- <-> clean turn on check results, which move no tip and so would leave
                                 -- this field the one cached answer nothing invalidates.
    author_login         TEXT,
    author_avatar        TEXT,
    author_url           TEXT,
    head_ref_name        TEXT,
    base_ref_name        TEXT,
    head_ref_oid         TEXT,
    review_request_count INTEGER,
    last_commit_status   TEXT,
    node_id              TEXT,   -- GraphQL node id (REST/webhook sources only)
    body                 TEXT,   -- PR description; NULL = GitHub null body (or GraphQL-sourced row)
    author_type          TEXT,   -- user.type: User | Bot | Organization
    base_ref_oid         TEXT,   -- base.sha
    head_repo_full_name  TEXT,   -- head.repo.full_name; NULL when the head repo is gone (deleted fork)
    auto_merge_method    TEXT,   -- native auto-merge method when armed (merge|squash|rebase); NULL = not armed
    merge_commit_sha     TEXT,   -- GitHub's test-merge sha; NULL until computed
    merge_stale_sha      TEXT,   -- the test-merge sha a base/head push invalidated. A tip change ALWAYS
                                 -- changes the sha of a SUCCESSFUL test merge (different parents), so a
                                 -- refetch re-offering THIS sha is presumed a pre-push answer (GitHub's
                                 -- recompute lag) and must not re-resolve mergeable. The exception is a
                                 -- CONFLICTED (dirty) PR: it gets NO new test merge -- GitHub retains the
                                 -- last-good sha and re-offers it with mergeable:false -- so a CONFLICTING
                                 -- same-sha answer is accepted once the marker outlives
                                 -- ghdata.MergeStaleConflictingWindow (the strftime '-30 seconds' in
                                 -- UpsertPullRequest). Cleared when an answer is accepted.
    merge_stale_at       TEXT,   -- when the push invalidated it. The marker only rejects within a bounded
                                 -- window (ghdata.MergeStaleTTL == the strftime '-1 hour' in
                                 -- UpsertPullRequest) -- the OUTER backstop, behind the tip proof below,
                                 -- so a sha wrongly marked stale -- absorbed post-recompute before the
                                 -- late push delivery landed -- cannot wedge the row into missing forever.
    merge_stale_ref      TEXT,   -- which branch the marking push moved (the PR's base or head name).
                                 -- With merge_stale_after it makes the marker VERIFIABLE -- the proof
                                 -- rule: an absorbed answer whose reported tip for this branch
                                 -- (base.sha / head.sha) equals merge_stale_after provably reflects the
                                 -- push, so it is accepted even when it re-offers the remembered sha --
                                 -- a wrongly-marked fresh sha heals on the very next poll instead of
                                 -- wedging for the whole window. Overwritten by every marking push
                                 -- (the newest answer must reflect the newest push).
    merge_stale_after    TEXT,   -- that push's after tip sha. NULL when the payload carried no usable
                                 -- after (empty / the all-zeros deleted-ref sha): no proof recorded, so
                                 -- only the MergeStaleTTL window unwedges a wrong mark (the old bound).
    touched_at           TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (owner, repo, number)
);

CREATE TABLE pr_labels (
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    pr_number   INTEGER NOT NULL,
    name        TEXT NOT NULL,
    color       TEXT NOT NULL,
    PRIMARY KEY (owner, repo, pr_number, name)
);

-- The record that a PR left the cache because it closed. Only OPEN PRs are
-- retained, so closing DELETES the row -- and once the row is gone there is
-- nothing for a later write carrying OLDER state to lose against: it simply
-- re-inserts the PR as open, and nothing afterwards restates the close.
-- A delivery's payload is the state at the moment the event happened, so any
-- delivery that arrives late (redelivered, or merely out of order -- GitHub
-- delivers concurrently) can be that write. This table is what lets the open
-- write paths refuse one. Keyed lowercase (NormalizeRepoKey), pruned at
-- ghdata.PRClosureRetention, and dropped the moment a write proves it
-- postdates the close (a genuine reopen).
CREATE TABLE pr_closures (
    owner       TEXT NOT NULL,     -- lowercased
    repo        TEXT NOT NULL,     -- lowercased
    number      INTEGER NOT NULL,
    updated_at  TEXT NOT NULL,     -- the closing view's pull_request.updated_at
    recorded_at TEXT NOT NULL,     -- RFC3339, for retention pruning
    PRIMARY KEY (owner, repo, number)
);

CREATE INDEX idx_pr_closures_recorded ON pr_closures (recorded_at);

-- Per-check state for a commit, fed by status/check_run/check_suite webhooks.
-- We aggregate these into the PR's last_commit_status rollup without re-fetching
-- from GitHub. context is the status context or check name (latest state wins).
CREATE TABLE commit_checks (
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    sha         TEXT NOT NULL,
    context     TEXT NOT NULL,
    state       TEXT NOT NULL,   -- normalized: SUCCESS / FAILURE / ERROR / PENDING / EXPECTED
    PRIMARY KEY (owner, repo, sha, context)
);

-- ============================================================================
-- Reveal-by-permission: grants and deny verdicts
-- ============================================================================

-- A grant records that GitHub itself proved a principal can read a repo:
--   - source 'list_sync': the repo appeared in an org-repos fetch made with
--     that principal's own token (replace-synced per principal+owner on every
--     sync; absence from a sync revokes list_sync grants).
--   - source 'probe': a repo-scoped cached-route fetch with the principal's
--     token answered 2xx.
-- Grants expire (expires_at = granted_at + the grant TTL) so revoked access
-- ages out; an authoritative 403 on a later fetch deletes the grant
-- immediately. A plain 404 does NOT revoke (it is ambiguous with
-- "resource missing inside an accessible repo").
CREATE TABLE access_grants (
    principal  TEXT NOT NULL,  -- "user:<id>", "app:<id>", "app-installation:<id>", or token fingerprint
    owner      TEXT NOT NULL,  -- lowercased
    repo       TEXT NOT NULL,  -- lowercased
    granted_at TEXT NOT NULL,  -- RFC3339
    expires_at TEXT NOT NULL,  -- RFC3339
    source     TEXT NOT NULL,  -- list_sync | probe
    PRIMARY KEY (principal, owner, repo)
);

CREATE INDEX idx_access_grants_repo ON access_grants (owner, repo);

-- A deny verdict caches GitHub's own authoritative "no" (404, or a
-- non-rate-limit 403) to ONE principal's probe of ONE resource, so an
-- unauthorized caller repeating the same request does not hammer GitHub.
-- Short TTL (~5m); only authoritative answers are stored -- transient
-- failures (5xx, 429, rate-limited 403) are never cached as denials. Keyed by
-- the exact resource (not the whole repo) because GitHub's 404 cannot be told
-- apart from "file/PR missing inside a repo the principal CAN see". Earning a
-- grant for the repo clears the principal's verdicts for it.
CREATE TABLE deny_cache (
    principal     TEXT NOT NULL,
    resource_kind TEXT NOT NULL,  -- contents | git_commit | repo_pulls | pull | repo_commits | compare
                                  -- | commit_status | check_runs | pull_files | branches | repo
    resource_key  TEXT NOT NULL,  -- route-specific resource key
    owner         TEXT NOT NULL,  -- lowercased
    repo          TEXT NOT NULL,  -- lowercased
    status        INTEGER NOT NULL,  -- 404 or 403
    message       TEXT NOT NULL DEFAULT '',
    denied_at     TEXT NOT NULL,  -- RFC3339
    expires_at    TEXT NOT NULL,  -- RFC3339
    PRIMARY KEY (principal, resource_kind, resource_key)
);

CREATE INDEX idx_deny_cache_repo ON deny_cache (principal, owner, repo);

-- ============================================================================
-- Cached-route response state (trimmed rebuilds; see internal/api/respcache.go)
-- ============================================================================
--
-- These tables back the cached REST routes. They store the STATE contained in a
-- GitHub response (never the raw response bytes); the API layer rebuilds a
-- trimmed response body from this state, dropping every URL field (url, *_url,
-- _links). Global truth like every other data table; who may read a row is the
-- reveal layer's job. Invalidation (webhook-driven) deletes rows outright.

-- State for GET /repos/{owner}/{repo}/contents/{path}?ref=... responses.
-- owner/repo are stored lowercased (GitHub treats them case-insensitively in
-- URLs, and webhook invalidation must match regardless of the caller's casing);
-- path and ref are exact. kind is 'file' (name/sha/size/encoding/content set),
-- 'dir' (entries = JSON array of trimmed {type,size,name,path,sha} objects), or
-- 'missing' (a cached 404; message = GitHub's error message). A 'missing' row
-- is only ever absorbed from a REVEALED principal's fetch (public repo or
-- grant held) -- an unauthorized probe's 404 is ambiguous and goes to
-- deny_cache instead, never into global truth.
CREATE TABLE contents_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    path         TEXT NOT NULL,              -- exact file path ('' never cached; route requires one)
    ref          TEXT NOT NULL DEFAULT '',   -- ?ref= query value ('' = default branch)
    kind         TEXT NOT NULL,              -- file | dir | missing
    name         TEXT NOT NULL DEFAULT '',
    sha          TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    encoding     TEXT NOT NULL DEFAULT '',
    content      TEXT NOT NULL DEFAULT '',   -- base64 content exactly as GitHub sent it
    entries      TEXT NOT NULL DEFAULT '',   -- dir listings: JSON array of trimmed entries
    message      TEXT NOT NULL DEFAULT '',   -- missing: GitHub's 404 message
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks invalidate sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_contents_cache_key ON contents_cache (owner, repo, path, ref);
CREATE INDEX idx_contents_cache_lru ON contents_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/git/commits/{sha} responses. A git commit
-- is immutable, so rows have no TTL and no webhook invalidation -- only LRU
-- pruning bounds them. Rows are written both by the API layer (on a fetch) and
-- by the webhook dispatcher (absorbed from push payload commits), and both must
-- rebuild to the same trimmed shape. parents is a comma-joined parent-sha list.
CREATE TABLE git_commits_cache (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    owner           TEXT NOT NULL,           -- lowercased
    repo            TEXT NOT NULL,           -- lowercased
    sha             TEXT NOT NULL,           -- lowercased full hex
    message         TEXT NOT NULL DEFAULT '',
    author_name     TEXT NOT NULL DEFAULT '',
    author_email    TEXT NOT NULL DEFAULT '',
    author_date     TEXT NOT NULL DEFAULT '',
    committer_name  TEXT NOT NULL DEFAULT '',
    committer_email TEXT NOT NULL DEFAULT '',
    committer_date  TEXT NOT NULL DEFAULT '',
    tree_sha        TEXT NOT NULL DEFAULT '',
    parents         TEXT NOT NULL DEFAULT '', -- comma-joined parent shas ('' = none)
    fetched_at      TEXT NOT NULL,            -- RFC3339
    last_used_at    TEXT NOT NULL             -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_git_commits_cache_key ON git_commits_cache (owner, repo, sha);
CREATE INDEX idx_git_commits_cache_lru ON git_commits_cache (last_used_at);

-- Per-page snapshots for GET /repos/{owner}/{repo}/commits (the commits LIST
-- route). The listed commits themselves are absorbed into the git_commits_cache
-- rows above (the same global truth the single git-commit route and push
-- payloads maintain); a snapshot row stores only the ORDERING/COMPLETENESS
-- proof for one exact modeled query shape: the response's commit shas, in
-- order, keyed by (owner, repo, ref_param, per_page, page) where ref_param is
-- the raw ?sha= query value ('' = default branch). A hit requires an unexpired
-- snapshot AND every listed sha still resolving in git_commits_cache (an
-- LRU-pruned commit degrades the snapshot to a miss). Listings are
-- ref-tip-relative and move on every push, so push/repository webhooks flush a
-- repo's snapshots (the absorbed commit rows are immutable and stay);
-- expires_at is the 24h TTL backstop for missed deliveries. owner/repo
-- lowercased like the other cached-route tables.
CREATE TABLE commits_list_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    ref_param    TEXT NOT NULL DEFAULT '',   -- raw ?sha= query value ('' = default branch)
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    shas         TEXT NOT NULL,              -- JSON array of commit shas in response order
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_commits_list_cache_key ON commits_list_cache (owner, repo, ref_param, per_page, page);
CREATE INDEX idx_commits_list_cache_lru ON commits_list_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/compare/{basehead} responses (the
-- three-dot base...head comparison pr-minder's auto_open_pr / close-empty
-- gates run per branch, every fleet sweep). One row per exact modeled request:
-- (owner, repo, basehead), where basehead is the raw base...head path tail
-- (branch names keep their slashes; the cross-fork owner:branch form is never
-- cached). base_ref/head_ref are the basehead's two sides (split at '...'),
-- stored separately so a push to ONE ref can flush exactly the comparisons
-- naming it on either side instead of the whole repo. status is the upstream
-- answer the row absorbed: 200 (a real comparison) or -- round 2 -- 404
-- ("unknown ref"), stored as an expiring miss marker so a fleet sweep probing
-- a deleted branch does not hammer GitHub; doc holds the rendered body either
-- way. For a 200, doc is the ALREADY-TRIMMED compare document as JSON --
-- {status, ahead_by, behind_by, total_commits, merge_base_commit:{sha},
-- commits:[...], files:[...]} with every URL field dropped and per-file patch
-- NEVER stored (no consumer reads it from compare; omitting it is also what
-- keeps rows modest -- the absorb cap bounds a row at a few hundred KB for a
-- huge comparison, most are a few KB). The presence/absence of the files
-- array is preserved exactly: pr-minder reads changed_files = files.length
-- and treats an ABSENT array as unknown (fail open), so the rebuild must
-- never invent or drop it. A comparison depends on both refs' tips, so a
-- push flushes the pushed ref's rows (base_ref or head_ref match; repo-wide
-- when the ref is unknown) and repository events flush the whole repo;
-- expires_at is the 24h TTL backstop for missed deliveries. base_tip_sha is
-- what makes a MISSED flush survivable on the side that matters: GitHub
-- states the base tip it computed against, so a row whose base branch has
-- since moved can be recognized as stale on read instead of being served.
-- The compare's
-- commits are also upserted into git_commits_cache on absorb (synergy with
-- the single-commit and commits-list routes); the doc is self-contained, so
-- a hit never depends on those rows. owner/repo lowercased like the other
-- cached-route tables.
CREATE TABLE compare_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    basehead     TEXT NOT NULL,              -- raw base...head path tail, exact
    base_ref     TEXT NOT NULL,              -- basehead's base side (before the '...')
    head_ref     TEXT NOT NULL,              -- basehead's head side (after the '...')
    base_tip_sha TEXT NOT NULL DEFAULT '',   -- base_commit.sha: the base tip this answer was computed against ('' = not stated)
    status       INTEGER NOT NULL DEFAULT 200, -- 200, or 404 (expiring unknown-ref miss marker)
    doc          TEXT NOT NULL,              -- rendered document as JSON (trimmed compare, or the 404 body)
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_compare_cache_key ON compare_cache (owner, repo, basehead);
CREATE INDEX idx_compare_cache_base_ref ON compare_cache (owner, repo, base_ref);
CREATE INDEX idx_compare_cache_head_ref ON compare_cache (owner, repo, head_ref);
CREATE INDEX idx_compare_cache_lru ON compare_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/commits/{ref}/status (the combined
-- commit status; kind='status'), GET .../commits/{ref}/check-runs (the check
-- runs for a ref; kind='check_runs'), and -- round 2 -- GET
-- .../commits/{ref}/statuses (the raw statuses LIST; kind='statuses_list')
-- -- one snapshot table for all three, since the routes share the key shape,
-- TTL, and flush triggers exactly. Rows are per pagination shape: round 2
-- added (per_page, page) to the key so paginated requests can be modeled; a
-- param-less request uses the defaults per_page=30, page=1. ref is stored
-- VERBATIM as requested (a branch name -- slashes and all -- a sha, or a
-- tag; NEVER resolved), so each spelling is its own row: a branch-form row
-- describes "that branch's tip at fetch time" and is flushed when the tip
-- can move. doc holds the ALREADY-TRIMMED document as JSON: for status
-- {state, sha, total_count, statuses:[{context, state, description,
-- created_at, updated_at}]}, for check_runs {total_count, check_runs:[{id,
-- head_sha, name, status, conclusion, started_at, completed_at, app:{id},
-- output:{title}, details_url, html_url}]}, for statuses_list the bare
-- trimmed array of {context, state, description, target_url, created_at,
-- updated_at}. URL fields are dropped EXCEPT the survey-pinned consumer-read
-- exceptions (2026-07-11 survey: the required-builds hook renders them) --
-- the statuses-list target_url and the check-run details_url/html_url;
-- everything else URL-ish stays dropped, incl. the combined status's
-- per-status target_url, and the check-run output is trimmed to {title}
-- (the unbounded summary/text never stored).
-- These rows deliberately do NOT read or write the commit_checks
-- truth table: its normalized per-context rows are lossy against these
-- responses (no timestamps, no descriptions, no run ids), so the snapshot is
-- kept whole; unifying the two is possible future work. status/check_run/
-- check_suite webhooks flush the payload-named refs' rows (the head
-- branch(es) plus the sha -- each spelling is its own row; repo-wide only
-- when the payload names none), push flushes the pushed ref's rows (its tip
-- moved; a brand-new sha has no rows yet anyway; repo-wide when the ref is
-- unknown), repository flushes the whole repo like everywhere else;
-- expires_at is the 24h TTL backstop for missed deliveries. owner/repo
-- lowercased like the other cached-route tables.
CREATE TABLE commit_ci_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    ref          TEXT NOT NULL,              -- verbatim ref path segment(s), never resolved
    kind         TEXT NOT NULL,              -- 'status' | 'check_runs' | 'statuses_list'
    per_page     INTEGER NOT NULL,           -- default 30 for param-less requests
    page         INTEGER NOT NULL,           -- default 1 for param-less requests
    doc          TEXT NOT NULL,              -- trimmed document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_commit_ci_cache_key ON commit_ci_cache (owner, repo, ref, kind, per_page, page);
CREATE INDEX idx_commit_ci_cache_lru ON commit_ci_cache (last_used_at);

-- Per-page snapshots for GET /repos/{owner}/{repo}/pulls/{number}/files (the
-- PR files listing). doc holds the ALREADY-TRIMMED JSON array -- per-file
-- {filename, status, additions, deletions, changes, previous_filename?,
-- patch?} with the presence/absence of previous_filename and patch preserved
-- exactly (consumers test for a string patch; binary/oversized files
-- legitimately lack one) and every URL field dropped. patch is unbounded, so
-- a rendered doc larger than 1 MiB is never stored (the request passes
-- through unstored). A PR's files move whenever its head or base moves, so
-- pull_request events flush that one PR's pages (head pushes -- including
-- fork heads whose pushes we never see -- base retargets, reopens) and
-- push/repository events flush the whole repo (the belt for missed
-- pull_request deliveries); expires_at is the 24h TTL backstop. owner/repo
-- lowercased like the other cached-route tables.
CREATE TABLE pull_files_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    number       INTEGER NOT NULL,
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed files array as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_pull_files_cache_key ON pull_files_cache (owner, repo, number, per_page, page);
CREATE INDEX idx_pull_files_cache_lru ON pull_files_cache (last_used_at);

-- Rendered-doc snapshots for the single-PR route's CLOSED/merged answers
-- (GET /repos/{owner}/{repo}/pulls/{number} where GitHub reports the PR
-- closed). The open-only invariant of the pull_requests truth table is
-- untouched: a fetched closed PR still deletes any cached open row, and
-- closed PRs live ONLY here, as trimmed documents rendered once at absorb
-- time from GitHub's own response (never re-derived). A closed PR only
-- changes via pull_request events (reopened/edited/relabeled), which flush
-- that one PR's doc; repository events flush the whole repo; a push is
-- deliberately NOT a flush -- it cannot mutate a closed PR. expires_at is the
-- 24h TTL backstop for missed deliveries, the same accepted staleness class
-- as PRRowFresh. owner/repo lowercased like the other cached-route tables.
CREATE TABLE closed_pull_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    number       INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed single-PR document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_closed_pull_cache_key ON closed_pull_cache (owner, repo, number);
CREATE INDEX idx_closed_pull_cache_lru ON closed_pull_cache (last_used_at);

-- Per-page snapshots for GET /repos/{owner}/{repo}/branches (the branches
-- listing). doc holds the ALREADY-TRIMMED JSON array -- per-branch {name,
-- commit:{sha}, protected} with commit.url and the protection object/URL
-- dropped. A listing moves whenever any branch is created, deleted, or its
-- tip advances -- all of which arrive as push events (a delete carries
-- deleted=true) -- so push/repository webhooks flush a repo's snapshots;
-- expires_at is the 24h TTL backstop for missed deliveries. owner/repo
-- lowercased like the other cached-route tables.
CREATE TABLE branches_list_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed branches array as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_branches_list_cache_key ON branches_list_cache (owner, repo, per_page, page);
CREATE INDEX idx_branches_list_cache_lru ON branches_list_cache (last_used_at);

-- Per-page snapshots for GET /repos/{owner}/{repo}/actions/runs?head_sha=...
-- (the workflow-runs listing filtered to ONE COMMIT -- pr-minder's
-- hasWorkflowRuns zombie probe, repeated per bot PR by the reconcile hook's
-- fleet sweeps). Whole-doc snapshots per exact pagination shape, keyed by
-- (owner, repo, head_sha, per_page, page); doc holds the ALREADY-TRIMMED
-- document as JSON.
--
-- Only the per-commit shape lives here. The repo-wide listing
-- (?status=&branch=) is NOT a snapshot at all -- it is rebuilt from the
-- workflow_runs truth table above, because no per-sha signal can name a
-- run entering or leaving `queued` and clearing a snapshot on every job
-- delivery is invalidate-and-refetch, which the cache doctrine forbids.
--
-- A sha's runs change when its CI moves, so status/check_run/check_suite/
-- workflow_job/workflow_run webhooks flush that sha's rows (workflow_job is
-- the precise signal -- its head_sha names the row directly; workflow_run is
-- the ONLY signal for a startup_failure run, which creates no jobs, check
-- runs, or statuses; repo-wide only when a payload carries no sha) and
-- repository events flush the whole repo; expires_at is the 24h TTL backstop
-- for missed deliveries and for run DELETION, which GitHub never delivers.
-- owner/repo lowercased like the other cached-route tables; head_sha
-- lowercased full hex.
CREATE TABLE workflow_runs_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    head_sha     TEXT NOT NULL,              -- lowercased full hex
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed runs document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_workflow_runs_cache_key ON workflow_runs_cache (owner, repo, head_sha, per_page, page);
CREATE INDEX idx_workflow_runs_cache_lru ON workflow_runs_cache (last_used_at);

-- Expiring 404 verdicts for GET /repos/{owner}/{repo}/git/commits/{sha}. The
-- git_commits_cache above never stores a 404 (a missing sha can be pushed
-- later), but pr-minder's mergeWouldBeEmpty re-reads a GC'd test-merge sha
-- FOREVER -- every fleet sweep re-probes the same permanently-missing object
-- -- so round 2 caches the miss itself, bounded by expires_at. doc holds the
-- rendered 404 body. A sha that later materializes is un-missed by the
-- absorb path: every real git-commit upsert clears this row (see
-- ghdata.upsertGitCommit), so the marker can never shadow a commit that now
-- exists. repository events flush the whole repo; owner/repo lowercased,
-- sha lowercased full hex.
CREATE TABLE git_commit_miss_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    sha          TEXT NOT NULL,              -- lowercased full hex
    doc          TEXT NOT NULL,              -- rendered 404 body as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 (miss markers always expire)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_git_commit_miss_cache_key ON git_commit_miss_cache (owner, repo, sha);
CREATE INDEX idx_git_commit_miss_cache_lru ON git_commit_miss_cache (last_used_at);

-- 406 "diff too large" verdicts for GET /repos/{owner}/{repo}/pulls/{number}
-- with the diff media type (pr-minder's getPullDiff probes the unified diff
-- first and falls back to paging the files API on a 406; an oversized PR
-- re-earns the same 406 on every describe hand-off). doc holds the rendered
-- 406 body. 200 diff bodies are NEVER stored -- that would be verbatim byte
-- caching, which the cache doctrine rejects; only the bounded negative
-- verdict is worth a row. Flushed per PR by pull_request/pull_request_review
-- events and repo-wide by push (a base push can move the three-dot diff
-- across the size boundary in either direction) and repository events;
-- expires_at is the 24h TTL backstop. owner/repo lowercased like the other
-- cached-route tables.
CREATE TABLE pull_diff406_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    number       INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- rendered 406 body as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 (miss markers always expire)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_pull_diff406_cache_key ON pull_diff406_cache (owner, repo, number);
CREATE INDEX idx_pull_diff406_cache_lru ON pull_diff406_cache (last_used_at);

-- State for POST /app/installations/{id}/access_tokens responses (the
-- installation-token mint cache). This table stays keyed by the verified app
-- identity ("app:<id>") -- it caches a CREDENTIAL minted for that app, not
-- GitHub state, so it is deliberately outside the global-truth model.
-- body_hash is the SHA-256 of the canonicalized request body (empty body vs
-- permissions/repositories subsets mint DIFFERENT tokens). token is a live
-- short-lived credential at rest -- same trust domain as the traffic itself,
-- bounded by expiry (see the security notes in CLAUDE.md). expires_at is the
-- serve-until time: GitHub's token expiry minus a safety buffer; past it the
-- row is a miss and a fresh mint replaces it.
CREATE TABLE install_token_cache (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    actor                TEXT NOT NULL,             -- "app:<verified app id>"
    installation_id      TEXT NOT NULL,             -- from the URL path
    body_hash            TEXT NOT NULL,             -- SHA-256 of canonicalized request body
    token                TEXT NOT NULL,             -- minted installation token (secret)
    token_expires_at     TEXT NOT NULL,             -- GitHub's expires_at, verbatim
    permissions          TEXT NOT NULL DEFAULT '',  -- JSON object, '' when GitHub omitted it
    repository_selection TEXT NOT NULL DEFAULT '',
    fetched_at           TEXT NOT NULL,             -- RFC3339
    expires_at           TEXT NOT NULL,             -- RFC3339 serve-until (token expiry - buffer)
    last_used_at         TEXT NOT NULL              -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_install_token_cache_key ON install_token_cache (actor, installation_id, body_hash);
CREATE INDEX idx_install_token_cache_install ON install_token_cache (installation_id);
CREATE INDEX idx_install_token_cache_lru ON install_token_cache (last_used_at);

-- "Open-PR list complete" markers for GET /repos/{owner}/{repo}/pulls (the
-- cached open-PR list). A valid row means: the GLOBAL pull_requests table
-- holds the repo's COMPLETE open-PR set (absorbed from a full REST list
-- response), so the route may rebuild the list from state. Webhook
-- pull_request events do NOT touch the marker -- they ARE the maintenance
-- (rows stay current); expires_at is only the TTL backstop bounding missed
-- deliveries. Who may READ the rebuilt list is the reveal layer's job. owner
-- and repo are stored lowercased, like the other cached-route tables.
CREATE TABLE pulls_list_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,   -- lowercased
    repo         TEXT NOT NULL,   -- lowercased
    fetched_at   TEXT NOT NULL,   -- RFC3339
    expires_at   TEXT NOT NULL,   -- RFC3339 TTL backstop (webhooks maintain rows, never the marker)
    last_used_at TEXT NOT NULL    -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_pulls_list_cache_key ON pulls_list_cache (owner, repo);
CREATE INDEX idx_pulls_list_cache_lru ON pulls_list_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/installation responses (an App-JWT-authed
-- endpoint, like the token mint: actor is the verified "app:<id>"). The answer
-- is app-specific -- each app has its own installations -- so this stays keyed
-- by app identity, deliberately outside the global-truth model. Invalidated by
-- installation/installation_repositories events for the stored installation
-- id, plus the TTL backstop. owner/repo lowercased.
--
-- status distinguishes the two absorbed answers: 200 rows carry a real
-- installation, 404 rows are the "not installed here" VERDICT and carry
-- installation_id 0, so the by-installation-id flush cannot reach them --
-- DeleteAbsentRepoInstallationCache is what clears those. A verdict's TTL is
-- deliberately much shorter than a 200's (installationAbsentTTL): the mirror
-- only receives ITS OWN App's installation webhooks, so a consumer App being
-- installed somewhere emits no signal the mirror can see.
CREATE TABLE repo_installation_cache (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    actor                TEXT NOT NULL,             -- "app:<verified app id>"
    owner                TEXT NOT NULL,             -- lowercased
    repo                 TEXT NOT NULL,             -- lowercased
    status               INTEGER NOT NULL DEFAULT 200, -- 200 installed | 404 absent verdict
    message              TEXT NOT NULL DEFAULT '',  -- GitHub's message, 404 rows only
    installation_id      INTEGER NOT NULL,
    account_login        TEXT NOT NULL DEFAULT '',
    account_type         TEXT NOT NULL DEFAULT '',  -- Organization | User
    repository_selection TEXT NOT NULL DEFAULT '',  -- all | selected
    app_id               INTEGER NOT NULL DEFAULT 0,
    app_slug             TEXT NOT NULL DEFAULT '',
    target_type          TEXT NOT NULL DEFAULT '',
    fetched_at           TEXT NOT NULL,             -- RFC3339
    expires_at           TEXT NOT NULL,             -- RFC3339 TTL backstop
    last_used_at         TEXT NOT NULL              -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_repo_installation_cache_key ON repo_installation_cache (actor, owner, repo);
CREATE INDEX idx_repo_installation_cache_install ON repo_installation_cache (installation_id);
CREATE INDEX idx_repo_installation_cache_lru ON repo_installation_cache (last_used_at);
CREATE INDEX idx_repo_installation_cache_status ON repo_installation_cache (status);

-- ============================================================================
-- Principal Identities (dashboard only)
-- ============================================================================
--
-- Maps a principal (the reveal-layer identity: "user:<id>", "app:<id>", or a
-- token fingerprint) to the GitHub login it authenticated as. Populated in
-- requireAuth whenever a token is validated. Purely for the dashboard: it
-- groups a signed-in user's principal under their login and lets an admin
-- attribute every principal. The raw token is never stored -- only the
-- principal key and the login GitHub reports for it.
CREATE TABLE actor_identities (
    actor       TEXT NOT NULL PRIMARY KEY,  -- principal key (matches access_grants.principal)
    login       TEXT NOT NULL,              -- GitHub login the credential authenticated as
    first_seen  TEXT NOT NULL,              -- RFC3339
    last_seen   TEXT NOT NULL               -- RFC3339
);

CREATE INDEX idx_actor_identities_login ON actor_identities (login);

-- ============================================================================
-- Webhook delivery log (dashboard observability)
-- ============================================================================

-- Every received webhook delivery and what the dispatcher did with it. Global
-- (one GitHub event = one row). Since v9 every stateful event applies straight
-- to global truth, so the old 'skipped' disposition ("no cache scope had this
-- repo") no longer exists; 'ignored' remains for genuinely untracked event
-- types/actions. delivery_id is the X-GitHub-Delivery UUID, which matches the
-- row in GitHub's "Recent Deliveries" UI, so the two can be lined up. The log
-- is capped to the most recent rows (see PruneWebhookDeliveries) since it is
-- observability, not source-of-truth.
CREATE TABLE webhook_deliveries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id  TEXT NOT NULL DEFAULT '',   -- X-GitHub-Delivery header (UUID)
    event_type   TEXT NOT NULL,              -- X-GitHub-Event header
    action       TEXT NOT NULL DEFAULT '',   -- payload "action", when present
    repo         TEXT NOT NULL DEFAULT '',   -- owner/name, when derivable
    received_at  TEXT NOT NULL,              -- RFC3339
    disposition  TEXT NOT NULL,              -- applied | invalidated | ignored | error
    detail       TEXT NOT NULL DEFAULT ''    -- human summary, e.g. "upserted PR #42"
);

-- The newest view of each SUBJECT this mirror has applied, so an older one
-- arriving later can be recognized as superseded rather than written over it.
-- GitHub does not order deliveries and never claims to; two views of one
-- resource can arrive in either order, and applying the older one second is a
-- correct, repeatable write of stale state (a merged PR came back OPEN 82
-- seconds after its merge exactly that way).
--
-- subject is the RESOURCE the view is of, at the grain that actually
-- supersedes -- "pr:owner/repo#12", "status:owner/repo:<sha>:<context>",
-- "check_run:owner/repo:<id>" (webhook.OrderOf). Coarser keys would let one
-- resource's event discard another's. event_time is the payload's own clock
-- for that subject, always a GitHub-set field, never a user-set one (a push
-- uses repository.pushed_at, never head_commit.timestamp).
--
-- Observability, not truth: the rows only gate writes, and losing them (a
-- nuke, a prune) costs at most a window in which an out-of-order delivery is
-- applied as it always was. Pruned by age on write.
CREATE TABLE webhook_watermarks (
    subject     TEXT PRIMARY KEY,           -- the resource, at superseding grain
    event_time  TEXT NOT NULL,              -- RFC3339, from the payload's own clock
    delivery_id TEXT NOT NULL DEFAULT '',   -- X-GitHub-Delivery of the view that set it
    event_type  TEXT NOT NULL DEFAULT '',   -- X-GitHub-Event of that view
    updated_at  TEXT NOT NULL               -- RFC3339 receipt time, for pruning
);

CREATE INDEX idx_webhook_watermarks_pruning ON webhook_watermarks (updated_at);

-- Deliveries this mirror has already asked GitHub to send again. GitHub keeps
-- its own log of deliveries it could not hand over and will re-send one on
-- request, but it never retries by itself -- so a delivery lost to a restart
-- or a blip is simply gone, and every cache that delivery would have moved
-- serves its last absorbed answer for the full TTL with nothing reporting a
-- gap. The replayer (internal/sync) reads that log and requests the missing
-- ones; this table is how it asks exactly once per delivery, across restarts.
-- One row per GitHub delivery id (a redelivery gets its own id, so a replay
-- that also fails is a new, separately-requestable delivery). Bounded by
-- pruning rows past the replayer's own lookback window.
CREATE TABLE webhook_replays (
    delivery_id  INTEGER PRIMARY KEY,        -- GitHub's numeric delivery id
    guid         TEXT NOT NULL DEFAULT '',   -- the X-GitHub-Delivery UUID it will arrive under
    event_type   TEXT NOT NULL DEFAULT '',
    delivered_at TEXT NOT NULL DEFAULT '',   -- RFC3339, GitHub's original attempt
    requested_at TEXT NOT NULL               -- RFC3339, when this mirror asked
);

CREATE INDEX idx_webhook_replays_requested ON webhook_replays (requested_at);

-- ============================================================================
-- Workflow jobs (webhook-fed Actions job state)
-- ============================================================================

-- GitHub Actions job state, fed by workflow_job webhooks (in_progress and
-- completed actions; queued/waiting churn is deliberately not recorded). Global
-- webhook-fed operational telemetry (the read path is admin-only). Empty string
-- means "not reported" for the optional TEXT fields, matching the
-- webhook_deliveries convention. Rows are bounded by pruning on write: completed
-- jobs older than a retention window are deleted after each upsert (see
-- PruneWorkflowJobs and ghdata.workflowJobRetention).
CREATE TABLE workflow_jobs (
    owner         TEXT NOT NULL,
    repo          TEXT NOT NULL,
    job_id        INTEGER NOT NULL,
    run_id        INTEGER NOT NULL DEFAULT 0,
    run_attempt   INTEGER NOT NULL DEFAULT 0,
    name          TEXT NOT NULL DEFAULT '',   -- job name
    workflow_name TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,              -- in_progress | completed
    conclusion    TEXT NOT NULL DEFAULT '',   -- success | failure | ... (completed only)
    head_sha      TEXT NOT NULL DEFAULT '',
    head_branch   TEXT NOT NULL DEFAULT '',
    html_url      TEXT NOT NULL DEFAULT '',
    started_at    TEXT NOT NULL DEFAULT '',   -- RFC3339
    completed_at  TEXT NOT NULL DEFAULT '',   -- RFC3339
    runner_name   TEXT NOT NULL DEFAULT '',   -- null in the payload until assigned
    updated_at    TEXT NOT NULL,              -- RFC3339: when the last webhook was applied
    PRIMARY KEY (owner, repo, job_id)
);

-- Makes the on-write prune (DELETE ... WHERE status='completed' AND
-- completed_at < cutoff) a single indexed scan of only the completed rows.
CREATE INDEX idx_workflow_jobs_completed_at
    ON workflow_jobs (completed_at) WHERE status = 'completed';

-- GitHub Actions RUN state -- global truth, maintained by webhooks the same
-- way pull_requests is, and the state the repo-wide runs LISTING
-- (GET /repos/{owner}/{repo}/actions/runs?status=&branch=) is rebuilt from.
--
-- This is a truth table, NOT a response snapshot: a delivery that moves one
-- run UPDATES THAT RUN'S ROW and leaves every other run served. The listing
-- is a filtered view over these rows, so a run entering or leaving `queued`
-- changes the answer without anything being cleared and re-fetched.
--
-- Who writes it:
--   * workflow_run deliveries -- the authoritative whole object (id, name,
--     status, conclusion, timestamps). The only signal for a run that creates
--     no jobs at all (a startup_failure, or one gated by a concurrency group).
--   * workflow_job deliveries -- these name the RUN (run_id, head_sha,
--     head_branch, workflow_name, run_attempt) but carry no run-level status,
--     so they may only ever establish a run's identity and RAISE its status
--     floor (a job in_progress proves its run is in_progress). They must
--     never conclude a run: the job set may be incomplete.
--   * REST absorbs -- every /actions/runs response upserts every run it
--     listed.
-- check_suite/check_run deliveries carry no run identity at all and
-- deliberately do not touch this table.
--
-- Empty string means "not reported" (the workflow_jobs convention) and is
-- rendered as JSON null: name, conclusion (until completed), and
-- run_started_at (until it starts) are exactly that case.
CREATE TABLE workflow_runs (
    owner          TEXT NOT NULL,              -- lowercased
    repo           TEXT NOT NULL,              -- lowercased
    run_id         INTEGER NOT NULL,
    run_attempt    INTEGER NOT NULL DEFAULT 0,
    name           TEXT NOT NULL DEFAULT '',
    head_sha       TEXT NOT NULL DEFAULT '',   -- lowercased full hex
    head_branch    TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL,              -- queued | in_progress | completed | waiting | ...
    conclusion     TEXT NOT NULL DEFAULT '',   -- success | failure | ... (completed only)
    html_url       TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT '',   -- RFC3339, GitHub's own
    updated_at     TEXT NOT NULL DEFAULT '',   -- RFC3339, GitHub's own
    run_started_at TEXT NOT NULL DEFAULT '',   -- RFC3339, null while queued
    touched_at     TEXT NOT NULL,              -- RFC3339: when the mirror last applied anything
    PRIMARY KEY (owner, repo, run_id)
);

-- The listing's ordering (newest first, GitHub's own) and the status/branch
-- filters it selects on.
CREATE INDEX idx_workflow_runs_listing ON workflow_runs (owner, repo, created_at DESC, run_id DESC);
CREATE INDEX idx_workflow_runs_head_sha ON workflow_runs (owner, repo, head_sha);
-- Bounds the table: settled runs are pruned on write once they are older than
-- the retention window (ghdata.workflowRunRetention).
CREATE INDEX idx_workflow_runs_settled ON workflow_runs (touched_at) WHERE status = 'completed';

-- The COMPLETENESS PROOF for serving the runs listing out of workflow_runs.
--
-- Rows alone cannot answer a list: truth holds the runs the mirror has seen,
-- which is not the same as every run GitHub would return. One row per
-- (owner, repo, canonical filter set) records that a page-1 response for
-- EXACTLY that filter came back SHORT -- fewer items than per_page -- which
-- proves truth then held every run matching it. Webhooks maintain the rows
-- from that point on, so the marker is never touched by a run delivery; that
-- is the whole point (the pulls_list_cache stance).
--
-- expires_at is deliberately SHORT. It bounds exactly one hole: a run that
-- enters the filter's set without any delivery naming it -- which needs the
-- App subscribed to workflow_run (see docs/webhooks/*), since a run with no
-- jobs yet emits no workflow_job. A queued-backlog answer is what a runner
-- coordinator provisions against, so that window is held to minutes.
-- repository events clear it outright.
CREATE TABLE workflow_runs_list_cache (
    owner      TEXT NOT NULL,                  -- lowercased
    repo       TEXT NOT NULL,                  -- lowercased
    filters    TEXT NOT NULL,                  -- canonical "k=v&k=v" of the modeled filters, '' = unfiltered
    fetched_at TEXT NOT NULL,                  -- RFC3339
    expires_at TEXT NOT NULL,                  -- RFC3339
    PRIMARY KEY (owner, repo, filters)
);

-- Snapshots for GET /repos/{owner}/{repo}/git/ref/{ref} -- the ref-to-tip
-- lookup (heads/<branch>, tags/<tag>). One row per (owner, repo, verbatim
-- requested ref): callers may spell the same branch three ways and each
-- spelling is its own row, so the per-ref push flush covers every spelling
-- (refSpellings in internal/sync/webhook_invalidate.go). status is 200 (a
-- real ref) or 404 (the "no such ref" VERDICT -- deleted branches are polled
-- forever by fleet sweeps, and ref creation arrives as a push, which clears
-- the verdict). doc holds the rendered trimmed body. owner/repo lowercased.
-- reconciled_against records the contradicting PR base.sha a refetch already
-- settled (see Store.GetCachedGitRefChecked): a PR's base.sha legitimately
-- LAGS its branch, so the same lagging value must not buy a refetch on every
-- read. Empty means no contradiction has been answered for this row.
CREATE TABLE git_ref_cache (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    owner              TEXT NOT NULL,              -- lowercased
    repo               TEXT NOT NULL,              -- lowercased
    ref                TEXT NOT NULL,              -- VERBATIM requested ref path, e.g. "heads/main"
    status             INTEGER NOT NULL,           -- 200 (a ref) or 404 (absent verdict)
    doc                TEXT NOT NULL,              -- trimmed ref document as JSON
    fetched_at         TEXT NOT NULL,              -- RFC3339
    expires_at         TEXT NOT NULL,              -- RFC3339 TTL backstop (pushes flush sooner)
    last_used_at       TEXT NOT NULL,              -- RFC3339, for LRU pruning
    reconciled_against TEXT NOT NULL DEFAULT ''    -- a contradicting PR base.sha this row's fetch already settled
);

CREATE UNIQUE INDEX idx_git_ref_cache_key ON git_ref_cache (owner, repo, ref);
CREATE INDEX idx_git_ref_cache_lru ON git_ref_cache (last_used_at);

-- Snapshots for the Actions JOB reads:
--   GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs  (kind 'run_jobs')
--   GET /repos/{owner}/{repo}/actions/jobs/{job_id}       (kind 'job')
--
-- A job still moving is stored too, on a short TTL, and REWRITTEN by its own
-- workflow_job deliveries rather than left to age (ghdata.ApplyWorkflowJob).
-- The fetch proves a page's membership; the deliveries keep its contents
-- current; the short TTL bounds a lost delivery and nothing else. Storing only
-- terminal answers left the GHA coordinator re-asking GitHub for state it had
-- already been told, which was most of the mirror's passthrough volume.
--
-- ref_id is the run id (kind 'run_jobs') or the job id (kind 'job'); run_id is
-- carried on BOTH kinds so a re-run -- a different set of jobs under the same
-- run id -- flushes every row it invalidates from one signal.
CREATE TABLE workflow_jobs_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    kind         TEXT NOT NULL,              -- 'run_jobs' | 'job'
    ref_id       INTEGER NOT NULL,           -- run id ('run_jobs') or job id ('job')
    run_id       INTEGER NOT NULL,           -- the owning run, on both kinds
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_workflow_jobs_cache_key ON workflow_jobs_cache (owner, repo, kind, ref_id, per_page, page);
CREATE INDEX idx_workflow_jobs_cache_run ON workflow_jobs_cache (owner, repo, run_id);
CREATE INDEX idx_workflow_jobs_cache_lru ON workflow_jobs_cache (last_used_at);

-- Snapshot for GET /repos/{owner}/{repo}/code-quality/setup -- GitHub Code
-- Quality's per-repo enablement configuration (public preview; schema
-- `code-quality-setup` in GitHub's OpenAPI description). One row per repo:
-- the endpoint takes no query parameters, so the repo IS the key.
--
-- This is CONFIG, changed only by an explicit PATCH to the same path or by a
-- human in the UI -- and GitHub emits no webhook for either. The mirror
-- flushes the row when it PROXIES such a PATCH, and `repository` events flush
-- repo-wide, but a change made outside the mirror is invisible until
-- expires_at. The TTL is short for that reason (codeQualitySetupTTL); see
-- docs/cache/rest-routes.md.
CREATE TABLE code_quality_setup_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    doc          TEXT NOT NULL,              -- trimmed setup document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL (the primary bound here)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_code_quality_setup_cache_key ON code_quality_setup_cache (owner, repo);
CREATE INDEX idx_code_quality_setup_cache_lru ON code_quality_setup_cache (last_used_at);

-- Snapshot for GET /repos/{owner}/{repo}/labels/{name} -- one label
-- definition. The name is the VERBATIM requested path segment (GitHub label
-- names carry spaces and punctuation), so a differently-cased spelling of the
-- same label is its own row; every flush here is repo-wide, which is what
-- makes that safe.
--
-- Only the 200 is stored. The absent answer deliberately is not: a caller's
-- ensure-then-create pass would read its own stale 404 in the seconds before
-- the `label` delivery lands, and in this fleet's traffic essentially every
-- answer is a 200 anyway, so the verdict would buy nothing for that risk.
--
-- Invalidation: EVERY `label` delivery (created/edited/deleted -- a rename
-- moves two names at once, so the grain is the repo, and these events are
-- rare), the write verbs the mirror proxies on the same path, `repository`
-- events, + a 24h TTL backstop.
CREATE TABLE label_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    name         TEXT NOT NULL,              -- VERBATIM requested label name
    doc          TEXT NOT NULL,              -- trimmed label document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_label_cache_key ON label_cache (owner, repo, name);
CREATE INDEX idx_label_cache_lru ON label_cache (last_used_at);

-- Snapshot for GET /installation/repositories -- "which repos does the token
-- I am holding cover".
--
-- Keyed by the CREDENTIAL (a SHA-256 fingerprint of the bearer, never the
-- bearer), not by the reveal-layer principal. The answer belongs to one
-- installation token, and the app:<id> principal deliberately shares one
-- bucket across every token of an app -- including tokens of DIFFERENT
-- installations, which see different repos. Per-credential keying is also
-- what gates the row: it can only ever be replayed to the exact credential
-- GitHub already answered.
--
-- Bounded primarily by its TTL (installationReposTTL): installation /
-- installation_repositories deliveries flush the whole table, but the mirror
-- only receives ITS OWN App's, and these callers are other apps.
CREATE TABLE installation_repos_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_fp     TEXT NOT NULL,              -- SHA-256 of the bearer, never the bearer
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed listing document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL (the primary bound here)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_installation_repos_cache_key ON installation_repos_cache (token_fp, per_page, page);
CREATE INDEX idx_installation_repos_cache_lru ON installation_repos_cache (last_used_at);

-- Snapshots for the webhook CONFIGURATION listings:
--   GET /repos/{owner}/{repo}/hooks  (scope 'repo')
--   GET /orgs/{org}/hooks            (scope 'org', repo '')
--
-- Keyed by the CREDENTIAL (a SHA-256 fingerprint of the bearer, never the
-- bearer), like installation_repos_cache and for a stronger reason: these are
-- ADMIN-only reads. The reveal layer proves READ access and its public fast
-- path admits any authenticated principal, so a global row behind the ordinary
-- gate would hand a read-only caller the repo's webhook URLs. Per-credential
-- keying is self-gating -- a row is only ever replayed to the exact credential
-- GitHub already answered it for -- and needs no new authorization machinery.
-- What a GLOBAL row would need instead, and the arithmetic that decides
-- whether it would pay, is in docs/cache/rest-routes.md.
--
-- Invalidation: the write verbs the mirror proxies on these paths flush the
-- listing ACROSS ALL CREDENTIALS (a hook one caller creates changes what every
-- caller sees), `repository` events flush a repo's rows, and the TTL is the
-- primary bound -- GitHub's `meta` event is NOT a usable signal here: it is
-- delivered only to the hook being deleted, so another hook's deletion is
-- something the mirror never hears about.
CREATE TABLE hooks_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_fp     TEXT NOT NULL,              -- SHA-256 of the bearer, never the bearer
    scope        TEXT NOT NULL,              -- 'repo' | 'org'
    owner        TEXT NOT NULL,              -- lowercased owner, or the org login
    repo         TEXT NOT NULL,              -- lowercased repo; '' for scope 'org'
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed hooks document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL (the primary bound here)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_hooks_cache_key ON hooks_cache (token_fp, scope, owner, repo, per_page, page);
CREATE INDEX idx_hooks_cache_target ON hooks_cache (scope, owner, repo);
CREATE INDEX idx_hooks_cache_lru ON hooks_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/git/trees/{tree_sha}[?recursive=1].
-- Trees are content-addressed and immutable like git commits: no TTL, no
-- webhook invalidation (no delivery names a tree object), LRU pruning only.
-- recursive is the verbatim query value ('' or '1') because GitHub answers a
-- DIFFERENT entry set for the same sha depending on it, so it is part of the
-- key rather than a rendering option. doc is the whole trimmed document
-- (sha, truncated, tree[] with url dropped from every entry) rendered once at
-- absorb time, the hooks_cache/branches_list_cache snapshot convention.
CREATE TABLE git_trees_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    sha          TEXT NOT NULL,              -- lowercased full hex tree sha
    recursive    TEXT NOT NULL DEFAULT '',   -- '' or '1', verbatim
    doc          TEXT NOT NULL,              -- trimmed tree document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_git_trees_cache_key ON git_trees_cache (owner, repo, sha, recursive);
CREATE INDEX idx_git_trees_cache_lru ON git_trees_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/check-runs/{check_run_id} (the SINGLE
-- check-run read, distinct from commit_ci_cache's LIST route). Keyed by the
-- check run's own id, which never changes owner/repo/sha once created.
-- Absorbed from an upstream fetch AND rewritten in place by `check_run`
-- webhook deliveries (ghdata.StoredCheckRun/TrimCheckRunJSON, the same trim
-- the LIST route uses -- one answer for what a check run becomes regardless
-- of source). doc is the trimmed single-check-run document. expires_at is a
-- backstop for a check run whose terminal state a delivery never reaches
-- (deleted app, GitHub-side ordering loss); webhooks are the primary path.
CREATE TABLE check_run_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    check_run_id INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed check-run document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks rewrite sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_check_run_cache_key ON check_run_cache (owner, repo, check_run_id);
CREATE INDEX idx_check_run_cache_lru ON check_run_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/git/matching-refs/heads/{prefix} (the
-- ref-prefix search pr-minder's queue-branch scan uses). doc is the trimmed
-- JSON array of matching {ref, object:{sha}} entries for one exact prefix.
-- A branch create/delete/tip-move all arrive as a push naming a ref under
-- refs/heads/ -- push/repository webhooks flush the whole repo's rows (the
-- branches_list_cache precedent: a prefix search has no narrower per-ref
-- target than the listing does). expires_at is the 24h TTL backstop.
CREATE TABLE matching_refs_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    prefix       TEXT NOT NULL,              -- verbatim path after heads/
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed matching-refs array as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_matching_refs_cache_key ON matching_refs_cache (owner, repo, prefix, per_page, page);
CREATE INDEX idx_matching_refs_cache_lru ON matching_refs_cache (last_used_at);

-- State for the identity/self routes with no per-repo scope and no webhook
-- signal: GET /app (App metadata, keyed by verified app id), GET /user and
-- GET /user/orgs (keyed by the bearer's fingerprint, the hooks_cache
-- convention -- a token uniquely names one user). `kind` picks which
-- question a row answers; doc is the trimmed document. TTL is the PRIMARY
-- bound for all three (hooks_cache's staleness stance): App metadata rarely
-- changes and carries no webhook at all, and GitHub sends no event for a
-- user's own profile edits or org membership changes (the CLAUDE.md
-- `organization`/`membership` payload-unused exception covers the same gap).
CREATE TABLE identity_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    subject_key  TEXT NOT NULL,              -- app:<id> or the bearer's token fingerprint
    kind         TEXT NOT NULL,              -- 'app' | 'user' | 'user_orgs'
    doc          TEXT NOT NULL,              -- trimmed document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL (the primary bound here)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_identity_cache_key ON identity_cache (subject_key, kind);
CREATE INDEX idx_identity_cache_lru ON identity_cache (last_used_at);

-- State for GET /orgs/{org}/actions/runners. Keyed by the bearer's
-- fingerprint like hooks_cache -- this is an admin-scoped read (needs
-- `manage_runners:organization` or equivalent) and GitHub sends no webhook
-- for a runner's online/offline/busy transitions, so a global row would both
-- leak admin-only data through the ordinary reveal gate and go stale with no
-- way to notice. TTL is short and primary: a runner's status is a live value
-- a scheduler provisions against.
CREATE TABLE org_runners_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_fp     TEXT NOT NULL,              -- SHA-256 of the bearer, never the bearer
    org          TEXT NOT NULL,              -- lowercased
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- trimmed runners document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL (the primary bound here)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_org_runners_cache_key ON org_runners_cache (token_fp, org, per_page, page);
CREATE INDEX idx_org_runners_cache_lru ON org_runners_cache (last_used_at);

-- State for GET /app/installations (every installation of the calling App --
-- distinct from repo_installation_cache, which answers "does the app cover
-- THIS repo/owner"). Keyed by the verified app id, per-page, cached VERBATIM
-- like identity_cache/org_runners_cache (identical-or-passthrough: this is a
-- listing of full installation objects with no consumer survey to trim
-- against). `installation`/`installation_repositories` deliveries flush the
-- owning app's pages when the payload's own installation object names its
-- app_id (installation_apply.go); expires_at is the TTL backstop otherwise.
CREATE TABLE app_installations_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    app_key      TEXT NOT NULL,              -- app:<id>
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- verbatim installations-page JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_app_installations_cache_key ON app_installations_cache (app_key, per_page, page);
CREATE INDEX idx_app_installations_cache_lru ON app_installations_cache (last_used_at);

-- State for GET /search/issues?q=...&per_page=...: an exact-query-string
-- cache, the documented exception to webhook-driven invalidation (see
-- docs/cache/uncacheable-routes.md) -- an arbitrary search query has no
-- single resource a webhook could name, so this is TTL-only and
-- deliberately short: long enough to collapse a tight poll loop asking the
-- identical query, short enough that a real answer change (an issue closed,
-- a label added) is visible within one poll cycle either way.
CREATE TABLE search_issues_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    query_key    TEXT NOT NULL,              -- sha256(q + '\n' + per_page + '\n' + page), verbatim query is unbounded
    doc          TEXT NOT NULL,              -- trimmed search-results document as JSON
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL (the primary and only bound)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_search_issues_cache_key ON search_issues_cache (query_key);
CREATE INDEX idx_search_issues_cache_lru ON search_issues_cache (last_used_at);
