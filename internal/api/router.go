// CORS is mounted before the auth group, so preflight is answered without a token.

package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/httpobs"
	"github.com/wow-look-at-my/github-state-mirror/internal/latency"
	"github.com/wow-look-at-my/github-state-mirror/internal/notify"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

func NewRouter(
	mgr *freshness.Manager,
	store *ghdata.Store,
	webhookSecret string,
	dispatcher *syncpkg.WebhookDispatcher,
	queue *webhook.Queue,
	gh *ghclient.Client,
	allowedOrigins []string,
	authSvc *auth.Service,
	baseURL string,
	checker *syncpkg.ConsistencyChecker,
	meter *ratemeter.Store,
	notifier *notify.Notifier,
	dbPath string,
	timeline *reqtimeline.Recorder,
	debouncer *Debouncer,
	app *ghclient.AppAuthenticator,
	lat *latency.Recorder,
	eng Engine,
) http.Handler {
	r := chi.NewRouter()
	r.Use(stampRequestStart)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// CORS is mounted before the auth group, so preflight is answered without a token.
	r.Use(corsMiddleware(allowedOrigins))
	// gh calls a non-github.com host with Enterprise paths; see ghecompat.go.
	r.Use(gheCompat)
	// No caller may cancel an Actions run through the mirror; see runcancel.go.
	r.Use(refuseRunCancel)
	// Every answer carries the caller's X-RateLimit-* headers; see ratelimit_headers.go.
	r.Use(rateLimitHeaders(meter))

	// reqlog: the dashboard "Requests" view; every request is also mirrored onto the Timeline.
	reqlog := newRequestLog()
	reqlog.timeline = timeline

	// shapes captures uncached-traffic shapes for the admin brief; see docs/dashboard/implementation-brief.md.
	shapes := newShapeStore()

	// ghProxy: the passthrough proxy, debounced; see docs/cache/-tier-contract.md.
	debouncer.attach(reqlog, timeline)
	ghProxy := recordPassthrough(debouncer.Wrap(newGitHubProxy(gh.BaseURL(), meter, func(resp *http.Response) {
		if resp.Request != nil {
			invalidateMintOnAuthFailure(resp.Request.Context(), store, bearerToken(resp.Request), resp)
		}
	}, TimelineProxyObserver(timeline))), reqlog, shapes)

	// recordIdentity: shared by requireAuth and the self-verifying app-JWT routes; see CLAUDE.md.
	recordIdentity := newIdentityRecorder(store)

	// A route the engine spec declares goes to the engine, behind requireAuth; see engine.go.
	if eng != nil {
		eng.SetRequestSink(reqlog.recordEngine)
	}
	r.Use(engineDispatch(eng, requireAuth(gh, recordIdentity)))

	// upstream is observed at its transport, so every call it makes reaches the Timeline.
	upstream := httpobs.Client(0, TimelineUpstreamObserver(timeline))
	h := &handlers{mgr: mgr, store: store, ghProxy: ghProxy, reqlog: reqlog, gh: gh, upstream: upstream, meter: meter, recordIdentity: recordIdentity, timeline: timeline, shapes: shapes}

	// Web dashboard: session-cookie authz, distinct from the data API below; see docs/dashboard/dashboard.md.
	var appEvents func(context.Context) ([]string, error)
	if app != nil {
		appEvents = app.SubscribedEvents
	}
	dash := newDashboard(authSvc, store, baseURL, reqlog, checker, meter, notifier, dbPath, timeline, shapes, appEvents, dispatcher.Ordering)
	dash.latency = lat
	dash.routes(r)

	// Webhook endpoint: HMAC-signed, outside requireAuth; see docs/webhooks/dispatch.md.
	r.Post("/webhook", webhook.Handler(webhookSecret, queue))

	// Update-liveness probes, outside requireAuth; see CLAUDE.md's "Update-check endpoints".
	registerUpdateChecks(r, store, mgr)

	// Scanner probes for paths GitHub does not have, answered with its a later own; see notgithub.go.
	for _, p := range notGitHubPaths {
		r.Get(p, h.notGitHub)
	}

	// GitHub OAuth relays for browser clients, outside requireAuth; see docs/oauth-relay.md.
	r.Post("/login/oauth/access_token", h.oauthAccessToken)
	r.Post("/login/device/code", h.oauthDeviceCode)

	// Installation-token mint + repo-installation lookup, App-JWT verified; see docs/cache/rest-routes.md.
	r.Post("/app/installations/{id}/access_tokens", h.cachedInstallationToken)
	r.Get("/repos/{owner}/{repo}/installation", h.cachedRepoInstallation)
	// Owner-level installation lookups, same App-JWT terms; see docs/cache/rest-routes.md.
	r.Get("/orgs/{org}/installation", h.cachedOwnerInstallation(ownerInstallScopeOrg, "org"))
	r.Get("/users/{username}/installation", h.cachedOwnerInstallation(ownerInstallScopeUser, "username"))

	// Cached App identity, same App-JWT terms; see docs/cache/rest-routes.md.
	r.Get("/app", h.cachedApp)
	r.Get("/app/installations", h.cachedAppInstallations)

	// Data endpoints: bearer-token required, -tiered cache contract; see CLAUDE.md.
	r.Group(func(r chi.Router) {
		r.Use(requireAuth(gh, recordIdentity))

		// Subscriber-notification CRUD, under the reserved /_mirror/* namespace; see docs/notifications.md.
		(&subscriptionsAPI{notifier: notifier}).routes(r)

		// GraphQL endpoint: only the org-repos query shape is cached; see docs/cache/-tier-contract.md.
		r.Post("/graphql", h.graphql)

		// Cached self routes: the token's own profile and org memberships; see docs/cache/rest-routes.md.
		r.Get("/user", h.cachedUser)
		r.Get("/user/orgs", h.cachedUserOrgs)

		// Rate-limit standing, answered from observed headers, never upstream; see docs/cache/rest-routes.md.
		r.Get("/rate_limit", h.cachedRateLimit)

		// Cached org self-hosted-runners listing, admin-scoped; see docs/cache/rest-routes.md.
		r.Get("/orgs/{org}/actions/runners", h.cachedOrgRunners)
		// Cached single runners and the repo listing, per credential; see docs/cache/rest-routes.md.
		r.Get("/orgs/{org}/actions/runners/{runner_id}", h.cachedOrgRunner)
		r.Get("/repos/{owner}/{repo}/actions/runners", h.cachedRepoRunners)
		r.Get("/repos/{owner}/{repo}/actions/runners/{runner_id}", h.cachedRepoRunner)

		// Cached personalized repo listing; see docs/cache/rest-routes.md.
		r.Get("/user/repos", h.cachedUserRepos)

		// Cached issue/PR search; see docs/cache/uncacheable-routes.md.
		r.Get("/search/issues", h.cachedSearchIssues)

		// Repo contents, then immutable git commits; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/contents/*", h.cachedContents)

		// Cached README read; the subtree spelling shares the handler and row space.
		r.Get("/repos/{owner}/{repo}/readme", h.cachedReadme)
		r.Get("/repos/{owner}/{repo}/readme/*", h.cachedReadme)

		r.Get("/repos/{owner}/{repo}/git/commits/{sha}", h.cachedGitCommit)

		// Cached git tree read, content-addressed and immutable; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/git/trees/{sha}", h.cachedGitTree)

		// Cached ref-prefix search; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/git/matching-refs/heads/*", h.cachedMatchingRefs)

		// Cached ref lookup, stored verbatim per spelling; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/git/ref/*", h.cachedGitRef)

		// Cached commits LIST, per-page sha snapshots; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/commits", h.cachedCommitsList)

		// The /commits/* subtree dispatcher (status/check-runs/statuses); see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/commits/*", h.commitsSubtree)

		// The statuses-list spelling, same handler and row space as above; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/statuses/*", h.statusesAlias)

		// Cached compare, the -dot base...head comparison; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/compare/*", h.cachedCompare)

		// Cached workflow-runs listing, head_sha required; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/actions/runs", h.cachedWorkflowRuns)
		// Cached single run, rebuilt from workflow_runs truth; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/actions/runs/{run_id}", h.cachedWorkflowRun)

		// Cached Actions WORKFLOW definitions, listing and single; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/actions/workflows", h.cachedWorkflowsList)
		r.Get("/repos/{owner}/{repo}/actions/workflows/{id}", h.cachedWorkflow)

		// Cached owner repo listings, keyed by the bearer's fingerprint; see docs/cache/rest-routes.md.
		r.Get("/orgs/{org}/repos", h.cachedOwnerRepos(ghdata.OwnerReposScopeOrg, "org"))
		r.Get("/users/{username}/repos", h.cachedOwnerRepos(ghdata.OwnerReposScopeUser, "username"))

		// Cached Actions JOB reads, a run's jobs page and a single job; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/actions/runs/{run_id}/jobs", h.cachedRunJobs)
		r.Get("/repos/{owner}/{repo}/actions/jobs/{job_id}", h.cachedWorkflowJob)

		// Cached log redirects, each until its signed location expires; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/actions/runs/{run_id}/logs", h.cachedRunLogs)
		r.Get("/repos/{owner}/{repo}/actions/jobs/{job_id}/logs", h.cachedJobLogs)

		// Cached SINGLE check-run read, keyed by the run's own id; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/check-runs/{check_run_id}", h.cachedCheckRun)
		// Cached check-run annotations, flushed by the run's own deliveries; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/check-runs/{check_run_id}/annotations", h.cachedCheckRunAnnotations)

		// Cached rules in force on a branch; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/rules/branches/*", h.cachedBranchRules)

		// Cached Code Quality setup; the PATCH flushes before proxying; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/code-quality/setup", h.cachedCodeQualitySetup)
		// Every write spelling flushes; a proxy-only method leaves a stale GET.
		r.Patch("/repos/{owner}/{repo}/code-quality/setup", h.patchCodeQualitySetup)
		r.Put("/repos/{owner}/{repo}/code-quality/setup", h.patchCodeQualitySetup)
		r.Delete("/repos/{owner}/{repo}/code-quality/setup", h.patchCodeQualitySetup)

		// Cached single-label read; the PATCH/DELETE flush before proxying; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/labels/{name}", h.cachedLabel)
		r.Patch("/repos/{owner}/{repo}/labels/{name}", h.writeLabel)
		r.Delete("/repos/{owner}/{repo}/labels/{name}", h.writeLabel)

		// Cached bare-repo read, rebuilt from the repos truth row; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}", h.cachedRepo)

		// Cached archive redirects; the ref may carry slashes. See docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/tarball", h.cachedArchive(ghdata.RepoAnswerKindTarball))
		r.Get("/repos/{owner}/{repo}/tarball/*", h.cachedArchive(ghdata.RepoAnswerKindTarball))
		r.Get("/repos/{owner}/{repo}/zipball", h.cachedArchive(ghdata.RepoAnswerKindZipball))
		r.Get("/repos/{owner}/{repo}/zipball/*", h.cachedArchive(ghdata.RepoAnswerKindZipball))

		// Cached branches list, per-page whole-doc snapshots; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/branches", h.cachedBranchesList)
		// Cached single branch; the name may carry slashes. See docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/branches/*", h.cachedBranch)

		// Cached org custom property values, keyed by the bearer's fingerprint; see docs/cache/rest-routes.md.
		r.Get("/orgs/{org}/properties/values", h.cachedOrgPropertyValues)

		// Cached installation-repositories listing, keyed by the bearer's fingerprint; see docs/cache/rest-routes.md.
		r.Get("/installation/repositories", h.cachedInstallationRepos)

		// Cached webhook CONFIGURATION listings, keyed by the bearer's fingerprint; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/hooks", h.cachedRepoHooks)
		r.Get("/repos/{owner}/{repo}/hooks/{hook_id}", h.cachedRepoHook)
		r.Get("/orgs/{org}/hooks/{hook_id}", h.cachedOrgHook)
		r.Post("/repos/{owner}/{repo}/hooks", h.writeRepoHooks)
		r.Patch("/repos/{owner}/{repo}/hooks/{hook_id}", h.writeRepoHooks)
		r.Delete("/repos/{owner}/{repo}/hooks/{hook_id}", h.writeRepoHooks)
		r.Get("/orgs/{org}/hooks", h.cachedOrgHooks)
		r.Post("/orgs/{org}/hooks", h.writeOrgHooks)
		r.Patch("/orgs/{org}/hooks/{hook_id}", h.writeOrgHooks)
		r.Delete("/orgs/{org}/hooks/{hook_id}", h.writeOrgHooks)

		// Cached PR routes: list, single PR, and files; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/pulls", h.cachedPullsList)
		r.Get("/repos/{owner}/{repo}/pulls/{number}", h.cachedPull)
		r.Get("/repos/{owner}/{repo}/pulls/{number}/files", h.cachedPullFiles)

		// Cached issue reads, single and listing; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/issues", h.cachedIssuesList)
		r.Get("/repos/{owner}/{repo}/issues/{number}", h.cachedIssue)

		// A PR's commits, reusing the repository commits list's storage; see docs/cache/rest-routes.md.
		r.Get("/repos/{owner}/{repo}/pulls/{number}/commits", h.cachedPullCommits)
	})

	// Fallback: forwarded to GitHub uncached; each tags WHY it forwarded. See docs/dashboard/dashboard.md.
	r.NotFound(taggedProxy(ghProxy, PassUnrouted))
	r.MethodNotAllowed(taggedProxy(ghProxy, PassMethod))

	return r
}
