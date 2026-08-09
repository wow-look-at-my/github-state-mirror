-- ============================================================================
-- Cached-route response state (contents / git commits / installation tokens)
-- ============================================================================
--
-- contents_cache and git_commits_cache are GLOBAL truth (one row per resource);
-- whether a caller may read a row is the reveal layer's job (see the
-- access_grants queries in ghdata.sql). install_token_cache and
-- repo_installations stay keyed by the verified app identity: they cache
-- app-specific answers (a minted credential; which installation covers a
-- repo), not shared GitHub state.

-- ---- contents_cache ----

-- name: GetContentsCache :one
SELECT * FROM contents_cache
WHERE owner = ? AND repo = ? AND path = ? AND ref = ?;

-- name: UpsertContentsCache :exec
INSERT INTO contents_cache (owner, repo, path, ref, kind, name, sha, size, encoding, content, entries, message, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, path, ref) DO UPDATE SET
    kind = excluded.kind,
    name = excluded.name,
    sha = excluded.sha,
    size = excluded.size,
    encoding = excluded.encoding,
    content = excluded.content,
    entries = excluded.entries,
    message = excluded.message,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchContentsCache :exec
UPDATE contents_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND path = ? AND ref = ?;

-- name: DeleteContentsCacheByRepo :exec
DELETE FROM contents_cache WHERE owner = ? AND repo = ?;

-- DeleteContentsCacheForRef drops one requested ref spelling's rows (an
-- empty ref = the default-branch rows) -- the per-ref push flush. A push to
-- branch X only moves X-relative answers, so other refs' rows survive.
-- name: DeleteContentsCacheForRef :exec
DELETE FROM contents_cache WHERE owner = ? AND repo = ? AND ref = ?;

-- name: DeleteExpiredContentsCache :exec
DELETE FROM contents_cache WHERE expires_at <= ?;

-- PruneContentsCacheLRU keeps only the most recently used rows. The subquery
-- selects everything beyond the newest `offset` rows by last_used_at; LIMIT -1
-- means "no limit" in SQLite, so OFFSET skips the keepers.
-- name: PruneContentsCacheLRU :exec
DELETE FROM contents_cache WHERE id IN (
    SELECT id FROM contents_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- git_commits_cache ----

-- name: GetGitCommitCache :one
SELECT * FROM git_commits_cache
WHERE owner = ? AND repo = ? AND sha = ?;

-- name: UpsertGitCommitCache :exec
INSERT INTO git_commits_cache (owner, repo, sha, message, author_name, author_email, author_date, committer_name, committer_email, committer_date, tree_sha, parents, fetched_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, sha) DO UPDATE SET
    message = excluded.message,
    author_name = excluded.author_name,
    author_email = excluded.author_email,
    author_date = excluded.author_date,
    committer_name = excluded.committer_name,
    committer_email = excluded.committer_email,
    committer_date = excluded.committer_date,
    tree_sha = excluded.tree_sha,
    parents = excluded.parents,
    fetched_at = excluded.fetched_at,
    last_used_at = excluded.last_used_at;

-- name: TouchGitCommitCache :exec
UPDATE git_commits_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND sha = ?;

-- name: PruneGitCommitsCacheLRU :exec
DELETE FROM git_commits_cache WHERE id IN (
    SELECT id FROM git_commits_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- commits_list_cache (per-page snapshots for the commits LIST route) ----

-- name: GetCommitsListCache :one
SELECT * FROM commits_list_cache
WHERE owner = ? AND repo = ? AND ref_param = ? AND per_page = ? AND page = ?;

-- name: UpsertCommitsListCache :exec
INSERT INTO commits_list_cache (owner, repo, ref_param, per_page, page, shas, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, ref_param, per_page, page) DO UPDATE SET
    shas = excluded.shas,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchCommitsListCache :exec
UPDATE commits_list_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND ref_param = ? AND per_page = ? AND page = ?;

-- DeleteCommitsListCacheByRepo drops a repo's snapshots -- the repository
-- webhook flush and the unparseable-push fallback (a push moves every
-- ref-relative listing). The absorbed git_commits_cache rows are immutable
-- and stay.
-- name: DeleteCommitsListCacheByRepo :exec
DELETE FROM commits_list_cache WHERE owner = ? AND repo = ?;

-- DeleteCommitsListCacheForRef drops one requested ref spelling's snapshots
-- (an empty ref = the default-branch listing) -- the per-ref push flush. The
-- absorbed git_commits_cache rows are immutable and stay.
-- name: DeleteCommitsListCacheForRef :exec
DELETE FROM commits_list_cache WHERE owner = ? AND repo = ? AND ref_param = ?;

-- name: DeleteExpiredCommitsListCache :exec
DELETE FROM commits_list_cache WHERE expires_at <= ?;

-- name: PruneCommitsListCacheLRU :exec
DELETE FROM commits_list_cache WHERE id IN (
    SELECT id FROM commits_list_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- compare_cache (GET /repos/{owner}/{repo}/compare/{basehead}) ----

-- name: GetCompareCache :one
SELECT * FROM compare_cache
WHERE owner = ? AND repo = ? AND basehead = ?;

-- name: UpsertCompareCache :exec
INSERT INTO compare_cache (owner, repo, basehead, base_ref, head_ref, status, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, basehead) DO UPDATE SET
    base_ref = excluded.base_ref,
    head_ref = excluded.head_ref,
    status = excluded.status,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchCompareCache :exec
UPDATE compare_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND basehead = ?;

-- DeleteCompareCacheByRepo drops a repo's compare docs -- the repository
-- webhook flush and the unparseable-push fallback (a push to either side of
-- any basehead can change the comparison).
-- name: DeleteCompareCacheByRepo :exec
DELETE FROM compare_cache WHERE owner = ? AND repo = ?;

-- DeleteCompareCacheForRef drops every comparison naming one ref on EITHER
-- side -- the per-ref push flush (callers pass the pushed ref twice, once
-- per side). Comparisons not touching the pushed ref survive.
-- name: DeleteCompareCacheForRef :exec
DELETE FROM compare_cache WHERE owner = ? AND repo = ? AND (base_ref = ? OR head_ref = ?);

-- name: DeleteExpiredCompareCache :exec
DELETE FROM compare_cache WHERE expires_at <= ?;

-- name: PruneCompareCacheLRU :exec
DELETE FROM compare_cache WHERE id IN (
    SELECT id FROM compare_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- commit_ci_cache (GET /repos/{owner}/{repo}/commits/{ref}/status,
-- ---- GET .../commits/{ref}/check-runs, and GET .../commits/{ref}/statuses) ----

-- name: GetCommitCICache :one
SELECT * FROM commit_ci_cache
WHERE owner = ? AND repo = ? AND ref = ? AND kind = ? AND per_page = ? AND page = ?;

-- name: UpsertCommitCICache :exec
INSERT INTO commit_ci_cache (owner, repo, ref, kind, per_page, page, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, ref, kind, per_page, page) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchCommitCICache :exec
UPDATE commit_ci_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND ref = ? AND kind = ? AND per_page = ? AND page = ?;

-- DeleteCommitCICacheByRepo drops ALL of a repo's commit-CI snapshots (every
-- kind, every ref, every page) -- the repository webhook flush and the
-- no-per-ref-signal fallbacks (unparseable push/check payloads).
-- name: DeleteCommitCICacheByRepo :exec
DELETE FROM commit_ci_cache WHERE owner = ? AND repo = ?;

-- DeleteCommitCICacheForRef drops one verbatim ref spelling's snapshots (all
-- kinds, all pages) -- the per-ref status/check_run/check_suite/push flush.
-- Other refs' snapshots survive: each spelling is its own row and the
-- payload names exactly which spellings moved.
-- name: DeleteCommitCICacheForRef :exec
DELETE FROM commit_ci_cache WHERE owner = ? AND repo = ? AND ref = ?;

-- name: DeleteExpiredCommitCICache :exec
DELETE FROM commit_ci_cache WHERE expires_at <= ?;

-- name: PruneCommitCICacheLRU :exec
DELETE FROM commit_ci_cache WHERE id IN (
    SELECT id FROM commit_ci_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- install_token_cache ----

-- name: GetInstallTokenCache :one
SELECT * FROM install_token_cache
WHERE actor = ? AND installation_id = ? AND body_hash = ?;

-- name: UpsertInstallTokenCache :exec
INSERT INTO install_token_cache (actor, installation_id, body_hash, token, token_expires_at, permissions, repository_selection, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, installation_id, body_hash) DO UPDATE SET
    token = excluded.token,
    token_expires_at = excluded.token_expires_at,
    permissions = excluded.permissions,
    repository_selection = excluded.repository_selection,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: DeleteInstallTokenCacheByInstallation :exec
DELETE FROM install_token_cache WHERE installation_id = ?;

-- DeleteInstallTokenCacheByToken drops the cached mint(s) that issued one
-- exact token -- the upstream-auth-failure invalidation: a proxied call
-- carrying this token came back 401/403, so the mint behind it must not
-- keep serving (its grants no longer match GitHub's).
-- name: DeleteInstallTokenCacheByToken :exec
DELETE FROM install_token_cache WHERE token = ?;

-- name: DeleteExpiredInstallTokenCache :exec
DELETE FROM install_token_cache WHERE expires_at <= ?;

-- name: PruneInstallTokenCacheLRU :exec
DELETE FROM install_token_cache WHERE id IN (
    SELECT id FROM install_token_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- pulls_list_cache (the "open-PR list complete" markers) ----

-- name: GetPullsListMarker :one
SELECT * FROM pulls_list_cache
WHERE owner = ? AND repo = ?;

-- name: UpsertPullsListMarker :exec
INSERT INTO pulls_list_cache (owner, repo, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (owner, repo) DO UPDATE SET
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchPullsListMarker :exec
UPDATE pulls_list_cache SET last_used_at = ?
WHERE owner = ? AND repo = ?;

-- DeletePullsListMarkersByRepo drops the marker on structural events
-- (repository renamed/deleted/etc.), NOT on pull_request events (those
-- maintain rows and leave the marker).
-- name: DeletePullsListMarkersByRepo :exec
DELETE FROM pulls_list_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredPullsListMarkers :exec
DELETE FROM pulls_list_cache WHERE expires_at <= ?;

-- name: PrunePullsListMarkersLRU :exec
DELETE FROM pulls_list_cache WHERE id IN (
    SELECT id FROM pulls_list_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- repo_installation_cache (GET /repos/{owner}/{repo}/installation) ----

-- name: GetRepoInstallationCache :one
SELECT * FROM repo_installation_cache
WHERE actor = ? AND owner = ? AND repo = ?;

-- name: UpsertRepoInstallationCache :exec
INSERT INTO repo_installation_cache (actor, owner, repo, status, message, installation_id, account_login, account_type, repository_selection, app_id, app_slug, target_type, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, repo) DO UPDATE SET
    status = excluded.status,
    message = excluded.message,
    installation_id = excluded.installation_id,
    account_login = excluded.account_login,
    account_type = excluded.account_type,
    repository_selection = excluded.repository_selection,
    app_id = excluded.app_id,
    app_slug = excluded.app_slug,
    target_type = excluded.target_type,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchRepoInstallationCache :exec
UPDATE repo_installation_cache SET last_used_at = ?
WHERE actor = ? AND owner = ? AND repo = ?;

-- name: DeleteRepoInstallationCacheByInstallation :exec
DELETE FROM repo_installation_cache WHERE installation_id = ?;

-- DeleteAbsentRepoInstallationCache drops every "not installed" VERDICT row.
-- Those carry installation_id 0, so the by-id flush above cannot reach them,
-- and an installation event means some account gained (or lost) an install --
-- exactly what a verdict claims is absent.
-- name: DeleteAbsentRepoInstallationCache :exec
DELETE FROM repo_installation_cache WHERE status <> 200;

-- name: DeleteExpiredRepoInstallationCache :exec
DELETE FROM repo_installation_cache WHERE expires_at <= ?;

-- name: PruneRepoInstallationCacheLRU :exec
DELETE FROM repo_installation_cache WHERE id IN (
    SELECT id FROM repo_installation_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- pull_files_cache (GET /repos/{owner}/{repo}/pulls/{number}/files) ----

-- name: GetPullFilesCache :one
SELECT * FROM pull_files_cache
WHERE owner = ? AND repo = ? AND number = ? AND per_page = ? AND page = ?;

-- name: UpsertPullFilesCache :exec
INSERT INTO pull_files_cache (owner, repo, number, per_page, page, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number, per_page, page) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchPullFilesCache :exec
UPDATE pull_files_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND number = ? AND per_page = ? AND page = ?;

-- DeletePullFilesCacheByRepo drops a repo's PR-files snapshots -- the
-- push/repository webhook flush (a push may have moved any same-repo PR's
-- head; the belt for missed pull_request deliveries).
-- name: DeletePullFilesCacheByRepo :exec
DELETE FROM pull_files_cache WHERE owner = ? AND repo = ?;

-- DeletePullFilesCacheByPR drops one PR's snapshots -- the pull_request event
-- flush (head pushed/synchronize -- including fork heads whose pushes we
-- never see -- base retargets, reopens).
-- name: DeletePullFilesCacheByPR :exec
DELETE FROM pull_files_cache WHERE owner = ? AND repo = ? AND number = ?;

-- name: DeleteExpiredPullFilesCache :exec
DELETE FROM pull_files_cache WHERE expires_at <= ?;

-- name: PrunePullFilesCacheLRU :exec
DELETE FROM pull_files_cache WHERE id IN (
    SELECT id FROM pull_files_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- closed_pull_cache (closed answers for GET /repos/{owner}/{repo}/pulls/{number}) ----

-- name: GetClosedPullCache :one
SELECT * FROM closed_pull_cache
WHERE owner = ? AND repo = ? AND number = ?;

-- name: UpsertClosedPullCache :exec
INSERT INTO closed_pull_cache (owner, repo, number, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchClosedPullCache :exec
UPDATE closed_pull_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND number = ?;

-- DeleteClosedPullCacheByRepo drops a repo's closed-PR docs -- the repository
-- webhook flush. A push is deliberately NOT a flush: it cannot mutate a
-- closed PR.
-- name: DeleteClosedPullCacheByRepo :exec
DELETE FROM closed_pull_cache WHERE owner = ? AND repo = ?;

-- DeleteClosedPullCacheByPR drops one PR's closed doc -- the pull_request
-- event flush (reopened/edited/relabeled), and the reopened-race safety
-- after an open absorb.
-- name: DeleteClosedPullCacheByPR :exec
DELETE FROM closed_pull_cache WHERE owner = ? AND repo = ? AND number = ?;

-- name: DeleteExpiredClosedPullCache :exec
DELETE FROM closed_pull_cache WHERE expires_at <= ?;

-- name: PruneClosedPullCacheLRU :exec
DELETE FROM closed_pull_cache WHERE id IN (
    SELECT id FROM closed_pull_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- branches_list_cache (GET /repos/{owner}/{repo}/branches) ----

-- name: GetBranchesListCache :one
SELECT * FROM branches_list_cache
WHERE owner = ? AND repo = ? AND per_page = ? AND page = ?;

-- name: UpsertBranchesListCache :exec
INSERT INTO branches_list_cache (owner, repo, per_page, page, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, per_page, page) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchBranchesListCache :exec
UPDATE branches_list_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND per_page = ? AND page = ?;

-- DeleteBranchesListCacheByRepo drops a repo's branches snapshots -- the
-- push/repository webhook flush (branch create, delete, and tip-move all
-- arrive as pushes).
-- name: DeleteBranchesListCacheByRepo :exec
DELETE FROM branches_list_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredBranchesListCache :exec
DELETE FROM branches_list_cache WHERE expires_at <= ?;

-- name: PruneBranchesListCacheLRU :exec
DELETE FROM branches_list_cache WHERE id IN (
    SELECT id FROM branches_list_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- workflow_runs_cache (GET /repos/{owner}/{repo}/actions/runs?head_sha=) ----

-- name: GetWorkflowRunsCache :one
SELECT * FROM workflow_runs_cache
WHERE owner = ? AND repo = ? AND head_sha = ? AND per_page = ? AND page = ?;

-- name: UpsertWorkflowRunsCache :exec
INSERT INTO workflow_runs_cache (owner, repo, head_sha, per_page, page, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, head_sha, per_page, page) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchWorkflowRunsCache :exec
UPDATE workflow_runs_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND head_sha = ? AND per_page = ? AND page = ?;

-- DeleteWorkflowRunsCacheByRepo drops a repo's workflow-runs snapshots --
-- the repository webhook flush and the sha-less payload fallback.
-- name: DeleteWorkflowRunsCacheByRepo :exec
DELETE FROM workflow_runs_cache WHERE owner = ? AND repo = ?;

-- DeleteWorkflowRunsCacheForHeadSHA drops one sha's snapshots (all pages) --
-- the per-sha status/check_run/check_suite/workflow_job flush. Other shas'
-- snapshots survive.
-- name: DeleteWorkflowRunsCacheForHeadSHA :exec
DELETE FROM workflow_runs_cache WHERE owner = ? AND repo = ? AND head_sha = ?;

-- ---- workflow_runs (truth) + workflow_runs_list_cache (completeness) ----

-- UpsertWorkflowRunFull records a run from an AUTHORITATIVE source -- a
-- workflow_run delivery or a REST absorb -- both of which carry the run's own
-- status and conclusion. Out-of-order tolerant on the run's own updated_at:
-- an older event never overwrites a newer one (a delivery replay, or a
-- listing absorbed while a fresher delivery was in flight).
-- name: UpsertWorkflowRunFull :exec
INSERT INTO workflow_runs (
    owner, repo, run_id, run_attempt, name, head_sha, head_branch,
    status, conclusion, html_url, created_at, updated_at, run_started_at, touched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, run_id) DO UPDATE SET
    run_attempt    = MAX(excluded.run_attempt, workflow_runs.run_attempt),
    name           = CASE WHEN excluded.name <> '' THEN excluded.name ELSE workflow_runs.name END,
    head_sha       = CASE WHEN excluded.head_sha <> '' THEN excluded.head_sha ELSE workflow_runs.head_sha END,
    head_branch    = CASE WHEN excluded.head_branch <> '' THEN excluded.head_branch ELSE workflow_runs.head_branch END,
    status         = CASE WHEN excluded.updated_at >= workflow_runs.updated_at THEN excluded.status ELSE workflow_runs.status END,
    conclusion     = CASE WHEN excluded.updated_at >= workflow_runs.updated_at THEN excluded.conclusion ELSE workflow_runs.conclusion END,
    html_url       = CASE WHEN excluded.html_url <> '' THEN excluded.html_url ELSE workflow_runs.html_url END,
    created_at     = CASE WHEN excluded.created_at <> '' THEN excluded.created_at ELSE workflow_runs.created_at END,
    updated_at     = MAX(excluded.updated_at, workflow_runs.updated_at),
    run_started_at = CASE WHEN excluded.run_started_at <> '' THEN excluded.run_started_at ELSE workflow_runs.run_started_at END,
    touched_at     = excluded.touched_at;

-- UpsertWorkflowRunFromJob records what a workflow_job delivery can prove
-- about its RUN. A job payload names the run's identity but carries no
-- run-level status, so this may only ever CREATE the row or RAISE its status
-- floor: a job that is in_progress proves the run is in_progress. It must
-- never conclude a run (the job set may be incomplete) and never regress one
-- that a workflow_run delivery or a REST absorb already settled -- hence the
-- status clause fires only when the stored status is 'queued' and the row was
-- never given a conclusion.
-- name: UpsertWorkflowRunFromJob :exec
INSERT INTO workflow_runs (
    owner, repo, run_id, run_attempt, name, head_sha, head_branch,
    status, conclusion, html_url, created_at, updated_at, run_started_at, touched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '', '', '', '', ?)
ON CONFLICT (owner, repo, run_id) DO UPDATE SET
    run_attempt = MAX(excluded.run_attempt, workflow_runs.run_attempt),
    name        = CASE WHEN workflow_runs.name = '' THEN excluded.name ELSE workflow_runs.name END,
    head_sha    = CASE WHEN workflow_runs.head_sha = '' THEN excluded.head_sha ELSE workflow_runs.head_sha END,
    head_branch = CASE WHEN workflow_runs.head_branch = '' THEN excluded.head_branch ELSE workflow_runs.head_branch END,
    status      = CASE WHEN workflow_runs.status = 'queued' AND workflow_runs.conclusion = '' AND excluded.status = 'in_progress'
                       THEN 'in_progress' ELSE workflow_runs.status END,
    touched_at  = excluded.touched_at;

-- ListWorkflowRuns returns one page of a repo's runs, newest first (GitHub's
-- own ordering), optionally filtered by status and/or head branch and/or head
-- sha. An empty filter argument means "no filter on this field".
-- The `status` filter matches a run's status OR its CONCLUSION, because
-- GitHub's own filter does: ?status=success selects completed runs that
-- succeeded, while ?status=queued selects by status. Matching only the status
-- column would answer ?status=success with nothing.
--
-- Every parameter here is NAMED on purpose: sqlc numbers `?` placeholders and
-- sqlc.arg() references in one shared sequence, so mixing the two silently
-- misaligns the bindings ("missing argument with index N" at run time).
-- name: ListWorkflowRuns :many
SELECT * FROM workflow_runs
WHERE owner = sqlc.arg(owner) AND repo = sqlc.arg(repo)
  AND (sqlc.arg(status) = '' OR status = sqlc.arg(status) OR conclusion = sqlc.arg(status))
  AND (sqlc.arg(head_branch) = '' OR head_branch = sqlc.arg(head_branch))
  AND (sqlc.arg(head_sha) = '' OR head_sha = sqlc.arg(head_sha))
ORDER BY created_at DESC, run_id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- CountWorkflowRuns is the same filter without pagination -- the listing's
-- total_count, exact because the completeness marker vouches for the set.
-- name: CountWorkflowRuns :one
SELECT COUNT(*) FROM workflow_runs
WHERE owner = sqlc.arg(owner) AND repo = sqlc.arg(repo)
  AND (sqlc.arg(status) = '' OR status = sqlc.arg(status) OR conclusion = sqlc.arg(status))
  AND (sqlc.arg(head_branch) = '' OR head_branch = sqlc.arg(head_branch))
  AND (sqlc.arg(head_sha) = '' OR head_sha = sqlc.arg(head_sha));

-- DeleteWorkflowRunsNotIn drops the rows a reconciling absorb proved stale:
-- runs that still MATCH the filter in truth but were absent from a short
-- page-1 response for that same filter. Their state provably moved and the
-- response did not say where to, so the row goes and the next read re-absorbs
-- it. sqlc.slice expands to the run ids the response did contain.
-- name: DeleteWorkflowRunsNotIn :exec
DELETE FROM workflow_runs
WHERE owner = sqlc.arg(owner) AND repo = sqlc.arg(repo)
  AND (sqlc.arg(status) = '' OR status = sqlc.arg(status) OR conclusion = sqlc.arg(status))
  AND (sqlc.arg(head_branch) = '' OR head_branch = sqlc.arg(head_branch))
  AND (sqlc.arg(head_sha) = '' OR head_sha = sqlc.arg(head_sha))
  AND run_id NOT IN (sqlc.slice(kept));

-- name: DeleteWorkflowRunsByRepo :exec
DELETE FROM workflow_runs WHERE owner = ? AND repo = ?;

-- PruneSettledWorkflowRuns bounds the table: completed runs the mirror has
-- not touched within the retention window are dropped. Runs still queued or
-- in progress are never pruned.
-- name: PruneSettledWorkflowRuns :exec
DELETE FROM workflow_runs WHERE status = 'completed' AND touched_at < ?;

-- name: GetWorkflowRunsListMarker :one
SELECT * FROM workflow_runs_list_cache WHERE owner = ? AND repo = ? AND filters = ?;

-- name: UpsertWorkflowRunsListMarker :exec
INSERT INTO workflow_runs_list_cache (owner, repo, filters, fetched_at, expires_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, filters) DO UPDATE SET
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at;

-- DeleteWorkflowRunsListMarkers clears a repo's completeness proofs. Only
-- repository events (and the expiry) do this -- a run delivery MAINTAINS the
-- rows the marker vouches for, so it must never clear the marker.
-- name: DeleteWorkflowRunsListMarkers :exec
DELETE FROM workflow_runs_list_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredWorkflowRunsListMarkers :exec
DELETE FROM workflow_runs_list_cache WHERE expires_at <= ?;

-- name: DeleteExpiredWorkflowRunsCache :exec
DELETE FROM workflow_runs_cache WHERE expires_at <= ?;

-- name: PruneWorkflowRunsCacheLRU :exec
DELETE FROM workflow_runs_cache WHERE id IN (
    SELECT id FROM workflow_runs_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- git_commit_miss_cache (expiring 404s for GET /repos/{owner}/{repo}/git/commits/{sha}) ----

-- name: GetGitCommitMissCache :one
SELECT * FROM git_commit_miss_cache
WHERE owner = ? AND repo = ? AND sha = ?;

-- name: UpsertGitCommitMissCache :exec
INSERT INTO git_commit_miss_cache (owner, repo, sha, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, sha) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchGitCommitMissCache :exec
UPDATE git_commit_miss_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND sha = ?;

-- name: DeleteGitCommitMissCacheByRepo :exec
DELETE FROM git_commit_miss_cache WHERE owner = ? AND repo = ?;

-- DeleteGitCommitMiss un-misses one sha. Every real git-commit upsert runs
-- it (ghdata.upsertGitCommit), so a sha that materializes -- pushed, listed,
-- or compared into git_commits_cache -- stops answering 404 immediately.
-- name: DeleteGitCommitMiss :exec
DELETE FROM git_commit_miss_cache WHERE owner = ? AND repo = ? AND sha = ?;

-- name: DeleteExpiredGitCommitMissCache :exec
DELETE FROM git_commit_miss_cache WHERE expires_at <= ?;

-- name: PruneGitCommitMissCacheLRU :exec
DELETE FROM git_commit_miss_cache WHERE id IN (
    SELECT id FROM git_commit_miss_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- pull_diff406_cache (406 verdicts for the single-PR diff read) ----

-- name: GetPullDiff406Cache :one
SELECT * FROM pull_diff406_cache
WHERE owner = ? AND repo = ? AND number = ?;

-- name: UpsertPullDiff406Cache :exec
INSERT INTO pull_diff406_cache (owner, repo, number, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchPullDiff406Cache :exec
UPDATE pull_diff406_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND number = ?;

-- DeletePullDiff406CacheByRepo drops a repo's 406 verdicts -- the
-- push/repository webhook flush (a base push can move a PR's three-dot diff
-- across the 406 size boundary in either direction).
-- name: DeletePullDiff406CacheByRepo :exec
DELETE FROM pull_diff406_cache WHERE owner = ? AND repo = ?;

-- DeletePullDiff406CacheForPR drops one PR's 406 verdict -- the
-- pull_request/pull_request_review event flush (a head push or retarget can
-- shrink the diff back under the boundary).
-- name: DeletePullDiff406CacheForPR :exec
DELETE FROM pull_diff406_cache WHERE owner = ? AND repo = ? AND number = ?;

-- name: DeleteExpiredPullDiff406Cache :exec
DELETE FROM pull_diff406_cache WHERE expires_at <= ?;

-- name: PrunePullDiff406CacheLRU :exec
DELETE FROM pull_diff406_cache WHERE id IN (
    SELECT id FROM pull_diff406_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- git_ref_cache (GET /repos/{owner}/{repo}/git/ref/{ref}) ----

-- name: GetGitRefCache :one
SELECT * FROM git_ref_cache
WHERE owner = ? AND repo = ? AND ref = ?;

-- name: UpsertGitRefCache :exec
INSERT INTO git_ref_cache (owner, repo, ref, status, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, ref) DO UPDATE SET
    status = excluded.status,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchGitRefCache :exec
UPDATE git_ref_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND ref = ?;

-- DeleteGitRefCacheForRef drops ONE requested spelling of a ref -- the
-- per-ref push flush (a push moves, creates, or deletes exactly one ref).
-- name: DeleteGitRefCacheForRef :exec
DELETE FROM git_ref_cache WHERE owner = ? AND repo = ? AND ref = ?;

-- name: DeleteGitRefCacheByRepo :exec
DELETE FROM git_ref_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredGitRefCache :exec
DELETE FROM git_ref_cache WHERE expires_at <= ?;

-- name: PruneGitRefCacheLRU :exec
DELETE FROM git_ref_cache WHERE id IN (
    SELECT id FROM git_ref_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- workflow_jobs_cache (Actions job reads) ----

-- name: GetWorkflowJobsCache :one
SELECT * FROM workflow_jobs_cache
WHERE owner = ? AND repo = ? AND kind = ? AND ref_id = ? AND per_page = ? AND page = ?;

-- name: UpsertWorkflowJobsCache :exec
INSERT INTO workflow_jobs_cache (owner, repo, kind, ref_id, run_id, per_page, page, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, kind, ref_id, per_page, page) DO UPDATE SET
    run_id = excluded.run_id,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchWorkflowJobsCache :exec
UPDATE workflow_jobs_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND kind = ? AND ref_id = ? AND per_page = ? AND page = ?;

-- DeleteWorkflowJobsCacheForRun drops every row a run's jobs back -- both the
-- run's own jobs pages and the single-job rows under it. A re-run replaces a
-- run's jobs under the SAME run id, so this is the flush that matters.
-- name: DeleteWorkflowJobsCacheForRun :exec
DELETE FROM workflow_jobs_cache WHERE owner = ? AND repo = ? AND run_id = ?;

-- name: DeleteWorkflowJobsCacheByRepo :exec
DELETE FROM workflow_jobs_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredWorkflowJobsCache :exec
DELETE FROM workflow_jobs_cache WHERE expires_at <= ?;

-- name: PruneWorkflowJobsCacheLRU :exec
DELETE FROM workflow_jobs_cache WHERE id IN (
    SELECT id FROM workflow_jobs_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- DeleteCommitsListCachePullSnapshots drops a repo's PR-commit snapshots --
-- the rows whose ref key is the synthetic "pull/<number>/commits" (see
-- internal/api/respcache_pullcommits.go). A push moves a PR's commit list
-- with no per-PR signal, exactly as it does the PR-files pages, so it flushes
-- these repo-wide as the belt behind the per-PR pull_request flush.
-- name: DeleteCommitsListCachePullSnapshots :exec
DELETE FROM commits_list_cache
WHERE owner = ? AND repo = ? AND ref_param LIKE 'pull/%';

-- ---- code_quality_setup_cache (GET /repos/{owner}/{repo}/code-quality/setup) ----

-- name: GetCodeQualitySetupCache :one
SELECT * FROM code_quality_setup_cache WHERE owner = ? AND repo = ?;

-- name: UpsertCodeQualitySetupCache :exec
INSERT INTO code_quality_setup_cache (owner, repo, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchCodeQualitySetupCache :exec
UPDATE code_quality_setup_cache SET last_used_at = ? WHERE owner = ? AND repo = ?;

-- name: DeleteCodeQualitySetupCacheByRepo :exec
DELETE FROM code_quality_setup_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredCodeQualitySetupCache :exec
DELETE FROM code_quality_setup_cache WHERE expires_at <= ?;

-- name: PruneCodeQualitySetupCacheLRU :exec
DELETE FROM code_quality_setup_cache WHERE id IN (
    SELECT id FROM code_quality_setup_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);
