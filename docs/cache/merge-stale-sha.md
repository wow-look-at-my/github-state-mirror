# The push-invalidated merge_stale_sha marker

The mechanism behind `internal/ghdata/respcache_pulls.go`'s `MergeStaleTTL`,
`MergeStaleConflictingWindow`, `mergeStaleMarkerLive`, `staleShaOffered`,
`PRMergeShaStale`, `pushProvenPostPush`, `conflictingPastReplicaLag`, and
`AbsorbSinglePull`'s stale-rejection path. Mirrors `UpsertPullRequest`'s SQL
guard in `queries/ghdata.sql`; keep the two in sync.

## MergeStaleTTL

`MergeStaleTTL` bounds how long a push-invalidated test-merge sha
(`merge_stale_sha`, stamped `merge_stale_at` by `NullPRMergeableByBranch`)
keeps rejecting re-offered answers as stale. Within the window a refetch
offering that exact sha is a pre-push answer (a base/head tip change always
changes the sha of a SUCCESSFUL test merge) and is stored unresolved — UNLESS
the answer carries the push-tip proof (`pushProvenPostPush`: its reported tip
for the marked branch equals the push's after sha), which demonstrates the
answer post-dates the push, OR it is a CONFLICTING answer past
`MergeStaleConflictingWindow` (a dirty PR retains its last-good sha and
legitimately re-offers it with `mergeable:false` — see that const). The tip
proof is what heals the wrong-mark race — a fetch absorbed AFTER GitHub's
recompute but BEFORE the (late) push delivery lands stores the FRESH sha,
which the push then wrongly marks stale — on the very next post-push-proven
absorb. The TTL is only the OUTER backstop behind both exemptions, for a
wrong mark whose answers never demonstrate the tip (a marker recorded without
a usable push after, or GitHub's reported tip lagging): past the window a
re-offered sha is accepted regardless. An hour is orders of magnitude above
GitHub's recompute lag under active polling (each rejected miss re-triggers
the recompute) while bounding that worst case.

`MergeStaleTTL` is a const, not a test-settable var, because the SAME window
is hardcoded in `queries/ghdata.sql` as the strftime `'-1 hour'` cutoffs
inside `UpsertPullRequest` — change both together. Tests age the MARKER
instead (`NullPRMergeableByBranch` takes the stamp time).

## MergeStaleConflictingWindow

`MergeStaleConflictingWindow` bounds how long the stale marker may reject a
CONFLICTING same-sha answer. The invariant behind the marker — a tip change
always changes the test-merge sha — holds only for SUCCESSFUL test merges: a
conflicted (dirty) PR gets NO new test merge, so GitHub keeps returning the
RETAINED last-good `merge_commit_sha` (and a `base.sha` frozen at the last
clean evaluation, which is why the push-tip proof cannot rescue this case)
alongside a fresh `mergeable:false`. Sha equality on a CONFLICTING answer is
therefore only evidence of pre-push-ness within possible GitHub read-replica
lag — seconds; 30s is a generous ~x10 margin. Past it, a CONFLICTING same-sha
answer is the dirty-retained pattern and MUST be accepted: rejecting it
wedged every conflicted PR to `mergeable:null` for the whole `MergeStaleTTL`
after EVERY base push (the pr-minder conflict-settle stall). MERGEABLE (and
unresolved) same-sha offers keep the full `MergeStaleTTL` rejection: a
successful test merge really does always mint a new sha, so resolved-true +
same sha => pre-push.

Like `MergeStaleTTL`, this is a const because the SAME window is hardcoded in
`queries/ghdata.sql` as the strftime `'-30 seconds'` cutoffs inside
`UpsertPullRequest` — change both together.

## mergeStaleMarkerLive

Reports whether the row carries a live push-invalidated-sha marker (both
columns set, stamp inside `MergeStaleTTL`). An unparseable stamp reads as
expired: fail open to the plain absorb.

## staleShaOffered

Reports whether `offered` is exactly the test-merge sha a recent push
invalidated on the existing row — presumed pre-push, because the push moved
the PR's base or head and a tip change always changes the sha of a
SUCCESSFUL test merge. Deliberately raw: the two exemptions that can overrule
the presumption — the push-tip proof (`pushProvenPostPush`) and the
dirty-retained CONFLICTING pattern (`conflictingPastReplicaLag`) — are
applied by the absorbing callers, never here.

## PRMergeShaStale

Reports whether the row's OWN `merge_commit_sha` is the push-invalidated
one. The guarded writes never store that state (the sha is nulled instead),
so this is belt and braces for the single-PR hit gate: a row that somehow
holds the provably-stale sha must miss, never serve it. Deliberately raw
equality — no push-tip proof consulted: a miss here just re-fetches, and the
ABSORB paths are where the proof decides.

## pushProvenPostPush

Reports whether the incoming doc provably post-dates the push that stamped
the existing row's stale marker: the marker remembers WHICH branch that push
moved (`merge_stale_ref`) and its post-push tip (`merge_stale_after`), so an
answer whose reported tip for that branch — `base_ref_oid` when the marked
ref is the base, `head_ref_oid` when it is the head — equals the push's
after sha reflects the push and cannot be the pre-push answer the marker
exists to reject. A marker recorded without the proof columns (no usable
push after) proves nothing, as does an answer whose reported tip is anything
else (older OR newer — only an exact match demonstrates; a mismatch keeps
the old reject-until-TTL behavior).

## conflictingPastReplicaLag

Reports whether the incoming answer is a CONFLICTING one offered against a
marker old enough that read-replica lag can no longer explain the same-sha
match: the dirty-retained pattern. A conflicted PR gets NO new test merge,
so GitHub re-offers the RETAINED last-good sha with a fresh `mergeable:false`
(and a `base.sha` frozen at the last clean evaluation, which is why
`pushProvenPostPush` cannot rescue it) — such an answer must be accepted or
every conflicted PR wedges to null after every base push. Within
`MergeStaleConflictingWindow` the answer could still be a genuinely pre-push
read served by a lagging replica, so the marker keeps rejecting; a
non-CONFLICTING answer never qualifies (a successful test merge always mints
a new sha).

## AbsorbSinglePull's stale-rejection path

`AbsorbSinglePull` upserts one fetched OPEN PR into global truth. Unlike the
COALESCE-ing webhook upsert, the fetched `mergeable` is authoritative —
including null ("GitHub is recomputing") — so it is force-set after the
upsert: a null answer must keep the single-PR route missing (and
re-fetching) until GitHub resolves it, never resurrect a stale value.

One answer is NOT authoritative: a response whose `merge_commit_sha` is the
exact sha a branch push just invalidated (a live `merge_stale_sha` marker).
The push moved the PR's base or head, and a tip change always changes the
sha of a SUCCESSFUL test merge — so GitHub re-offering the invalidated sha
means its recompute hasn't landed and the WHOLE answer (resolved mergeable
included) predates the push. Such an answer is stored UNRESOLVED (mergeable
NULL, `merge_commit_sha` NULL, marker kept), so reads keep missing — each one
re-triggering the recompute — until GitHub serves a NEW sha, which clears the
marker. TWO exemptions overrule that presumption and accept the answer, sha
and all, marker cleared: (1) the push-tip proof (`pushProvenPostPush`) — the
answer's reported tip for the marked branch equals the marking push's after
sha, so it demonstrably post-dates the push — which heals a WRONG mark (the
race where the fresh post-push answer was absorbed before the late push
delivery, which then stamped it stale) on the very next poll; and (2) the
dirty-retained pattern (`conflictingPastReplicaLag`) — a CONFLICTED PR gets
NO new test merge, so GitHub legitimately re-offers the RETAINED last-good
sha with `mergeable:false` forever, and once the marker outlives
`MergeStaleConflictingWindow` that is the only remaining explanation. The
upsert's SQL guard nulls the columns; the Go check here exists because the
authoritative force-set below would otherwise resurrect the rejected value,
and so the route can serve the response unresolved too. `staleRejected`
reports that outcome to the caller.
