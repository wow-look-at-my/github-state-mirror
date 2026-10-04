# The push-invalidated merge_stale_sha marker

The mechanism behind `internal/ghdata/respcache_pulls.go`'s `MergeStaleTTL`, `MergeStaleConflictingWindow`, `mergeStaleMarkerLive`, `staleShaOffered`, `PRMergeShaStale`, `pushProvenPostPush`, `conflictingPastReplicaLag`, and `AbsorbSinglePull`'s stale-rejection path. Mirrors `UpsertPullRequest`'s SQL guard in `queries/ghdata.sql`. Keep the two in sync.

## MergeStaleTTL

`MergeStaleTTL` bounds how long a push-invalidated test-merge sha (`merge_stale_sha`, stamped `merge_stale_at` by `NullPRMergeableByBranch`) keeps rejecting re-offered answers as stale. Within the window a refetch offering that exact sha is a pre-push answer (a base/head tip change always changes the sha of a SUCCESSFUL test merge) and is stored unresolved. This is UNLESS the answer carries the push-tip proof (`pushProvenPostPush`: its reported tip for the marked branch equals the push's after sha), which demonstrates the answer post-dates the push, OR it is a CONFLICTING answer. This is past `MergeStaleConflictingWindow` (a dirty PR retains its last-good sha and legitimately re-offers it with `mergeable:false` — see that const). The tip proof is what heals the wrong-mark race — a fetch absorbed AFTER GitHub's recompute but BEFORE the (late). Push delivery lands stores the FRESH sha, which the push then wrongly marks stale — on the very next post-push-proven absorb. The TTL is only the OUTER backstop behind both exemptions, for a wrong mark whose answers never demonstrate the tip (a marker recorded without a usable push after, or GitHub's reported tip lagging). Past the window a re-offered sha is accepted regardless. An hour is orders of magnitude above GitHub's recompute lag under active polling (each rejected miss re-triggers the recompute) while bounding that worst case.

`MergeStaleTTL` is a const, not a test-settable var, because the SAME window is hardcoded in `queries/ghdata.sql` as the strftime `'-1 hour'` cutoffs inside `UpsertPullRequest`. This is change both together. Tests age the MARKER instead (`NullPRMergeableByBranch` takes the stamp time).

## MergeStaleConflictingWindow

`MergeStaleConflictingWindow` bounds how long the stale marker may reject a CONFLICTING same-sha answer. The invariant behind the marker — a tip change always changes the test-merge sha — holds only for SUCCESSFUL test merges. A conflicted (dirty) PR gets NO new test merge, so GitHub keeps returning the RETAINED last-good `merge_commit_sha` (and a `base.sha` frozen at the last clean evaluation, which is why the push-tip proof cannot rescue this case) alongside a fresh `mergeable:false`. Sha equality on a CONFLICTING answer is therefore only evidence of pre-push-ness within possible GitHub read-replica lag — seconds. 30s is a generous ~x10 margin. Past it, a CONFLICTING same-sha answer is the dirty-retained pattern and MUST be accepted. Rejecting it wedged every conflicted PR to `mergeable:null` for the whole `MergeStaleTTL` after EVERY base push (the pr-minder conflict-settle stall). MERGEABLE (and unresolved) same-sha offers keep the full `MergeStaleTTL` rejection. A successful test merge really does always mint a new sha, so resolved-true + same sha => pre-push.

Like `MergeStaleTTL`, this is a const because the SAME window is hardcoded in `queries/ghdata.sql` as the strftime `'-30 seconds'` cutoffs inside `UpsertPullRequest` — change both together.

## mergeStaleMarkerLive

Reports whether the row carries a live push-invalidated-sha marker (both columns set, stamp inside `MergeStaleTTL`). An unparseable stamp reads as expired: fail open to the plain absorb.

## staleShaOffered

Reports whether `offered` is exactly the test-merge sha a recent push invalidated on the existing row — presumed pre-push. This is because the push moved the PR's base or head and a tip change always changes the sha of a SUCCESSFUL test merge. Deliberately raw: the exemptions that can overrule the presumption — the push-tip proof (`pushProvenPostPush`) and the dirty-retained CONFLICTING pattern (`conflictingPastReplicaLag`) — are applied. This is by the absorbing callers, not here.

## PRMergeShaStale

Reports whether the row's OWN `merge_commit_sha` is the push-invalidated one. The guarded writes never store that state (the sha is nulled instead). As a result, this is belt and braces for the single-PR hit gate. A row that somehow holds the provably-stale sha must miss, not serve it. Deliberately raw equality — no push-tip proof consulted: a miss here just re-fetches. The ABSORB paths are where the proof decides.

## pushProvenPostPush

Reports whether the incoming doc provably post-dates the push that stamped the existing row's stale marker. The marker remembers WHICH branch that push moved (`merge_stale_ref`) and its post-push tip (`merge_stale_after`), so an answer whose reported tip for that branch. This is `base_ref_oid` when the marked ref is the base, `head_ref_oid` when it is the head — equals the push's after sha reflects the push. This is cannot be the pre-push answer the marker exists to reject. A marker recorded without the proof columns (no usable push after) proves nothing, as does an answer whose reported tip is anything else (older OR newer. This is only an exact match demonstrates. A mismatch keeps the old reject-until-TTL behavior).

## conflictingPastReplicaLag

Reports whether the incoming answer is a CONFLICTING one offered against a marker old enough that read-replica lag can no longer explain. This is the same-sha match: the dirty-retained pattern. A conflicted PR gets NO new test merge, so GitHub re-offers the RETAINED last-good sha with a fresh `mergeable:false` (and a `base.sha` frozen at the last clean evaluation, which is why `pushProvenPostPush` cannot rescue it). Such an answer must be accepted or every conflicted PR wedges to null after every base push. Within `MergeStaleConflictingWindow` the answer can still be a genuinely pre-push read served by a lagging replica. As a result, the marker keeps rejecting. A non-CONFLICTING answer never qualifies (a successful test merge always mints a new sha).

## AbsorbSinglePull's stale-rejection path

`AbsorbSinglePull` upserts one fetched OPEN PR into global truth. Unlike the COALESCE-ing webhook upsert, the fetched `mergeable` is authoritative — including null ("GitHub is recomputing") — so it is force-set after the upsert. A null answer must keep the single-PR route missing (and re-fetching) until GitHub resolves it, not resurrect a stale value.

One answer is NOT authoritative: a response whose `merge_commit_sha` is the exact sha a branch push just invalidated (a live `merge_stale_sha` marker). The push moved the PR's base or head, and a tip change always changes the sha of a SUCCESSFUL test merge. So GitHub re-offering the invalidated sha means its recompute has not landed and the WHOLE answer (resolved mergeable included) predates the push.

Such an answer is stored UNRESOLVED (mergeable NULL, `merge_commit_sha` NULL, marker kept), so reads keep missing — each one re-triggering the recompute — until GitHub serves a NEW sha. This clears the marker. Exemptions overrule that presumption and accept the answer, sha and all, marker cleared: (1) the push-tip proof (`pushProvenPostPush`). The answer's reported tip for the marked branch equals the marking push's after sha.

As a result, it demonstrably post-dates the push — which heals a WRONG mark (the race where the fresh post-push answer was absorbed before the late push delivery, which then stamped it stale) on the very next poll. And (2) the dirty-retained pattern (`conflictingPastReplicaLag`) — a CONFLICTED PR gets NO new test merge. As a result, GitHub legitimately re-offers the RETAINED last-good sha with `mergeable:false` forever. This is once the marker outlives `MergeStaleConflictingWindow` that is the only remaining explanation.

The upsert's SQL guard nulls the columns. The Go check here exists because the authoritative force-set below will otherwise resurrect the rejected value. So the route can serve the response unresolved too. `staleRejected` reports that outcome to the caller.
