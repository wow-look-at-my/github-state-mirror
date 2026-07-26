package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/notify"
)

// Subscriber-notification CRUD: /_mirror/subscriptions.
//
// The top-level /_mirror/* prefix is the RESERVED mirror-native namespace.
// GitHub's API has no underscore-prefixed top-level paths, and chi-registered
// routes always win over the NotFound passthrough proxy, so nothing under
// /_mirror/* can ever collide with (or leak to) proxied GitHub traffic. New
// mirror-native endpoints belong under this prefix.
//
// The routes are registered INSIDE requireAuth, so callers resolve to a
// principal exactly like data routes (user:<id>, app:<id> via
// X-Mirror-Identity, or a token fingerprint) and every subscription is owned
// by the principal that created it. They deliberately do NOT go through the
// reveal layer (they are not repo reads) and are not recorded in the GitHub
// request log (only cached-route handlers and the passthrough record there).
type subscriptionsAPI struct {
	notifier *notify.Notifier
}

func (s *subscriptionsAPI) routes(r chi.Router) {
	r.Post("/_mirror/subscriptions", s.create)
	r.Get("/_mirror/subscriptions", s.list)
	r.Get("/_mirror/subscriptions/{id}", s.get)
	r.Patch("/_mirror/subscriptions/{id}", s.patch)
	r.Delete("/_mirror/subscriptions/{id}", s.remove)
}

// subscriptionJSON is the API view of a subscription. There is deliberately
// NO secret field — the stored HMAC key is never returned by any response.
// Principal is populated only on the admin operator view.
type subscriptionJSON struct {
	ID        string `json:"id"`
	Principal string `json:"principal,omitempty"`
	// PrincipalName is Principal's recorded display name (from
	// actor_identities); admin view only, like Principal.
	PrincipalName       string   `json:"principal_name,omitempty"`
	URL                 string   `json:"url"`
	Repos               []string `json:"repos"`
	Events              []string `json:"events"`
	Active              bool     `json:"active"`
	ConsecutiveFailures int64    `json:"consecutive_failures"`
	DisabledReason      string   `json:"disabled_reason"`
	CreatedAt           string   `json:"created_at"`
	UpdatedAt           string   `json:"updated_at"`
	LastSuccessAt       string   `json:"last_success_at"`
	LastFailureAt       string   `json:"last_failure_at"`
	LastError           string   `json:"last_error"`
}

func subscriptionView(sub notify.Subscription, includePrincipal bool) subscriptionJSON {
	v := subscriptionJSON{
		ID:                  sub.ID,
		URL:                 sub.URL,
		Repos:               sub.RepoFilters,
		Events:              sub.EventFilters,
		Active:              sub.Active,
		ConsecutiveFailures: sub.ConsecutiveFailures,
		DisabledReason:      sub.DisabledReason,
		CreatedAt:           sub.CreatedAt,
		UpdatedAt:           sub.UpdatedAt,
		LastSuccessAt:       sub.LastSuccessAt,
		LastFailureAt:       sub.LastFailureAt,
		LastError:           sub.LastError,
	}
	if v.Repos == nil {
		v.Repos = []string{}
	}
	if v.Events == nil {
		v.Events = []string{}
	}
	if includePrincipal {
		v.Principal = sub.Principal
	}
	return v
}

// begin resolves the store and the caller's principal, answering the error
// itself when either is unavailable.
func (s *subscriptionsAPI) begin(w http.ResponseWriter, r *http.Request) (*notify.Store, string, bool) {
	st := s.notifier.Store()
	if st == nil {
		http.Error(w, "subscriptions are not available on this server", http.StatusServiceUnavailable)
		return nil, "", false
	}
	principal := actor.FromContext(r.Context())
	if principal == "" {
		// requireAuth always sets a principal; this is a wiring failure.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, "", false
	}
	return st, principal, true
}

// maxSubscriptionBody bounds a create/patch request body.
const maxSubscriptionBody = 64 << 10

// decodeBody decodes a bounded JSON request body into v, answering 400 itself
// on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxSubscriptionBody)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// writeSubscriptionError maps a store error onto the HTTP response: 400 for a
// validation failure (with the message), 409 for the per-principal cap, 404
// for a missing/foreign id, 500 otherwise.
func writeSubscriptionError(w http.ResponseWriter, err error) {
	var ve *notify.ValidationError
	switch {
	case errors.As(err, &ve):
		http.Error(w, ve.Error(), http.StatusBadRequest)
	case errors.Is(err, notify.ErrLimitExceeded):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, notify.ErrNotFound):
		http.Error(w, "subscription not found", http.StatusNotFound)
	default:
		slog.Warn("subscriptions: store operation failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("json encode failed", "error", err)
	}
}

// create registers a new subscription for the caller's principal.
// POST /_mirror/subscriptions {url, secret, repos?, events?} -> 201.
func (s *subscriptionsAPI) create(w http.ResponseWriter, r *http.Request) {
	st, principal, ok := s.begin(w, r)
	if !ok {
		return
	}
	var in notify.NewSubscription
	if !decodeBody(w, r, &in) {
		return
	}
	sub, err := st.Create(r.Context(), principal, in, time.Now())
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, subscriptionView(sub, false))
}

// list returns the caller's own subscriptions.
// GET /_mirror/subscriptions -> {"subscriptions": [...]}.
func (s *subscriptionsAPI) list(w http.ResponseWriter, r *http.Request) {
	st, principal, ok := s.begin(w, r)
	if !ok {
		return
	}
	subs, err := st.List(r.Context(), principal)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	views := make([]subscriptionJSON, 0, len(subs))
	for _, sub := range subs {
		views = append(views, subscriptionView(sub, false))
	}
	writeJSON(w, map[string]any{"subscriptions": views})
}

// get returns one of the caller's subscriptions; a foreign principal's id
// answers 404 (no existence leak).
func (s *subscriptionsAPI) get(w http.ResponseWriter, r *http.Request) {
	st, principal, ok := s.begin(w, r)
	if !ok {
		return
	}
	sub, err := st.Get(r.Context(), principal, chi.URLParam(r, "id"))
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, subscriptionView(sub, false))
}

// patch partially updates one of the caller's subscriptions. Setting
// active=true resets consecutive_failures and clears disabled_reason (the
// re-enable path after an auto-disable).
func (s *subscriptionsAPI) patch(w http.ResponseWriter, r *http.Request) {
	st, principal, ok := s.begin(w, r)
	if !ok {
		return
	}
	var p notify.Patch
	if !decodeBody(w, r, &p) {
		return
	}
	sub, err := st.Update(r.Context(), principal, chi.URLParam(r, "id"), p, time.Now())
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, subscriptionView(sub, false))
}

// remove deletes one of the caller's subscriptions -> 204; missing/foreign -> 404.
func (s *subscriptionsAPI) remove(w http.ResponseWriter, r *http.Request) {
	st, principal, ok := s.begin(w, r)
	if !ok {
		return
	}
	if err := st.Delete(r.Context(), principal, chi.URLParam(r, "id")); err != nil {
		writeSubscriptionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- admin observability (dashboard-authenticated, JSON only) ----

type notificationsResponse struct {
	Counters      notify.Counters    `json:"counters"`
	Recent        []notify.Attempt   `json:"recent"`
	Subscriptions []subscriptionJSON `json:"subscriptions"`
}

// handleNotifications returns the notifier's in-memory delivery activity
// (counters + recent attempts; resets on restart, the request-log stance) and
// EVERY subscription with its principal — the operator view. Principals are
// decorated with their recorded display names (actor_identities, best-effort:
// a failed lookup just leaves names absent). Secrets are structurally absent
// (subscriptionJSON has no secret field). Admin-only, like the other global
// operator views.
func (d *dashboard) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	counters, recent := d.notifier.Activity(200)
	if recent == nil {
		recent = []notify.Attempt{}
	}
	subs, err := d.notifier.AllSubscriptions(r.Context())
	if err != nil {
		slog.Warn("list subscriptions failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	names := d.actorNames(r.Context())
	for i := range recent {
		recent[i].PrincipalName = names[recent[i].Principal]
	}
	views := make([]subscriptionJSON, 0, len(subs))
	for _, sub := range subs {
		v := subscriptionView(sub, true)
		v.PrincipalName = names[sub.Principal]
		views = append(views, v)
	}
	writeJSON(w, notificationsResponse{Counters: counters, Recent: recent, Subscriptions: views})
}
