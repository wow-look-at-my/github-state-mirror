// Package tlwire prototypes wire formats for GET /api/timeline: the same
// event stream encoded as JSON, BSON, protobuf, a hand-rolled row format and
// a hand-rolled columnar format, each measured raw and under gzip/zstd/br.
package tlwire

import (
	"fmt"
	"math/rand"
	"time"
)

// Event mirrors reqtimeline.Event exactly (field names, json tags, omitempty).
type Event struct {
	ID          uint64    `json:"id" bson:"id"`
	Kind        string    `json:"kind" bson:"kind"`
	Lane        string    `json:"lane" bson:"lane"`
	Start       time.Time `json:"start" bson:"start"`
	DurMs       int64     `json:"dur_ms" bson:"dur_ms"`
	Disposition string    `json:"disposition,omitempty" bson:"disposition,omitempty"`

	EventType  string `json:"event_type,omitempty" bson:"event_type,omitempty"`
	Action     string `json:"action,omitempty" bson:"action,omitempty"`
	DeliveryID string `json:"delivery_id,omitempty" bson:"delivery_id,omitempty"`
	Repo       string `json:"repo,omitempty" bson:"repo,omitempty"`

	Method    string `json:"method,omitempty" bson:"method,omitempty"`
	Route     string `json:"route,omitempty" bson:"route,omitempty"`
	Status    int    `json:"status,omitempty" bson:"status,omitempty"`
	Actor     string `json:"actor,omitempty" bson:"actor,omitempty"`
	ActorName string `json:"actor_name,omitempty" bson:"actor_name,omitempty"`

	Detail string `json:"detail,omitempty" bson:"detail,omitempty"`

	Target  string `json:"target,omitempty" bson:"target,omitempty"`
	Attempt int    `json:"attempt,omitempty" bson:"attempt,omitempty"`
	Final   bool   `json:"final,omitempty" bson:"final,omitempty"`
}

// ---- realistic corpus ----------------------------------------------------
//
// Shaped after what the mirror actually records (internal/api/requestgroups.go
// route shapes, the webhook event types the dispatcher handles, the reveal /
// upstream / internal dispositions). Cardinalities matter more than exact
// proportions here: every candidate format's size is dominated by how many
// DISTINCT strings the window holds and how repetitive the numeric columns are.

var routeShapes = []string{
	"/repos/{owner}/{repo}/pulls",
	"/repos/{owner}/{repo}/pulls/{number}",
	"/repos/{owner}/{repo}/pulls/{number}/files",
	"/repos/{owner}/{repo}/commits",
	"/repos/{owner}/{repo}/commits/{ref}/status",
	"/repos/{owner}/{repo}/commits/{ref}/check-runs",
	"/repos/{owner}/{repo}/statuses/{ref}",
	"/repos/{owner}/{repo}/compare/{basehead}",
	"/repos/{owner}/{repo}/contents/{path}",
	"/repos/{owner}/{repo}/git/commits/{sha}",
	"/repos/{owner}/{repo}/branches",
	"/repos/{owner}/{repo}/actions/runs",
	"/repos/{owner}/{repo}/installation",
	"/repos/{owner}/{repo}",
	"/app/installations/{id}/access_tokens",
	"/graphql",
	"/user",
	"/rate_limit",
}

var webhookTypes = []string{
	"push", "pull_request", "check_run", "check_suite", "status",
	"workflow_job", "workflow_run", "label", "repository",
	"installation", "pull_request_review", "installation_repositories",
}

var (
	requestDisp = []string{"hit", "miss", "passthrough", "upstream", "internal", "probe", "write", "error", "relay"}
	webhookDisp = []string{"applied", "invalidated", "ignored", "error"}
	methods     = []string{"GET", "GET", "GET", "GET", "GET", "GET", "GET", "POST", "PATCH"}
	statuses    = []int{200, 200, 200, 200, 200, 200, 200, 201, 204, 304, 404, 403, 422, 500, 502}
	actions     = []string{"", "opened", "closed", "synchronize", "completed", "created", "requested", "labeled"}
)

// Corpus is one generated window of events plus the parameters it was built
// with, so a report can state what it measured.
type Corpus struct {
	Events []Event
	Owners int
	Repos  int
	Actors int
}

// Generate builds n events spread over span, with realistic string
// cardinality. Deterministic for a given seed.
func Generate(n int, span time.Duration, seed int64) Corpus {
	rng := rand.New(rand.NewSource(seed))

	owners := []string{"wow-look-at-my", "PazerOP", "read-only-reference", "pr-minder", "buildhost-org"}
	repos := make([]string, 0, 220)
	for i := 0; i < 220; i++ {
		repos = append(repos, fmt.Sprintf("%s/%s-%d", owners[i%len(owners)], []string{"dummy-repo", "actions", "js-snippets", "go-toolchain", "webhook-runner", "buildhost"}[i%6], i))
	}
	actors := make([]string, 0, 34)
	names := make([]string, 0, 34)
	for i := 0; i < 30; i++ {
		actors = append(actors, fmt.Sprintf("app-installation:%d", 40000000+i*137))
		names = append(names, fmt.Sprintf("installation-account-%d", i))
	}
	actors = append(actors, "user:11347985", "app:1352104", "token:9f2a1c4e7b03", "anonymous")
	names = append(names, "PazerOP", "pr-minder", "", "")

	start := time.Now().UTC().Add(-span)
	step := span / time.Duration(n)

	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		// Events arrive in bursts, not on a metronome: jitter each step.
		at := start.Add(time.Duration(i) * step).Add(time.Duration(rng.Int63n(int64(step)+1)) - step/2)
		e := Event{ID: uint64(i + 1), Start: at, DurMs: skewedDur(rng)}
		switch roll := rng.Intn(100); {
		case roll < 84: // inbound requests + the mirror's own exchanges
			ai := rng.Intn(len(actors))
			e.Kind = "request"
			e.Method = methods[rng.Intn(len(methods))]
			e.Route = routeShapes[rng.Intn(len(routeShapes))]
			e.Lane = e.Method + " " + e.Route
			e.Status = statuses[rng.Intn(len(statuses))]
			e.Disposition = requestDisp[rng.Intn(len(requestDisp))]
			e.Actor = actors[ai]
			e.ActorName = names[ai]
		case roll < 95: // webhook deliveries — note the unique delivery GUIDs
			t := webhookTypes[rng.Intn(len(webhookTypes))]
			e.Kind = "webhook"
			e.Lane = "⇐ " + t
			e.EventType = t
			e.Action = actions[rng.Intn(len(actions))]
			e.DeliveryID = guid(rng)
			e.Repo = repos[rng.Intn(len(repos))]
			e.Disposition = webhookDisp[rng.Intn(len(webhookDisp))]
		default: // outbound subscriber notifications
			e.Kind = "notify"
			e.Lane = "⇒ notify"
			e.Target = []string{"webhook-runner.pazer.io", "hooks.example.com", "pr-minder.pazer.workers.dev"}[rng.Intn(3)]
			e.Status = []int{200, 200, 202, 500, 0}[rng.Intn(5)]
			e.Attempt = 1 + rng.Intn(3)
			e.Final = rng.Intn(3) != 0
			e.Disposition = []string{"applied", "error"}[rng.Intn(2)]
		}
		out = append(out, e)
	}
	return Corpus{Events: out, Owners: len(owners), Repos: len(repos), Actors: len(actors)}
}

// skewedDur models real durations: most exchanges are single-digit ms, a tail
// runs into seconds.
func skewedDur(rng *rand.Rand) int64 {
	switch r := rng.Intn(100); {
	case r < 45:
		return int64(rng.Intn(5))
	case r < 85:
		return int64(5 + rng.Intn(120))
	case r < 98:
		return int64(120 + rng.Intn(900))
	default:
		return int64(1000 + rng.Intn(9000))
	}
}

const hexDigits = "0123456789abcdef"

func guid(rng *rand.Rand) string {
	b := make([]byte, 36)
	for i := range b {
		b[i] = hexDigits[rng.Intn(16)]
	}
	b[8], b[13], b[18], b[23] = '-', '-', '-', '-'
	return string(b)
}
