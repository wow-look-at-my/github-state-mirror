package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file is the data API's front door: who is asking. router.go is the
// route table that mounts it.

// requireAuth enforces that every data request carries a usable GitHub token.
// It resolves the token's identity against GitHub (rejecting absent, malformed,
// or revoked credentials with), injects the token into the request context,
// and scopes all cache operations to a per-USER partition.
//
// The cache partition (actor) is "user:<numeric GitHub user id>" — GitHub
// user == cache scope (operator decision, --). All of a user's
// tokens (rotating sandbox PATs, OAuth logins, narrow and broad PATs alike)
// share warm, webhook-fed bucket, so a user is never isolated from
// themselves just because their tokens rotate. The numeric id (not the login)
// keys the bucket because ids survive login renames and are never recycled.
// Accepted trade-off: ANY token of a user reads what any of that user's tokens
// cached, including private-repo data cached by a broader-scoped token.
// DISTINCT users remain fully isolated from each other, and requests must
// never fall through to the service's own credentials (the GitHub App used for
// background refreshes), which may have far broader access than the caller.
//
// A token that is definitively NOT a user — GET /user answers /, e.g. a
// GitHub App installation token — keeps the old per-token fingerprint
// partition (and the verdict is cached per token). When the identity cannot be
// resolved at all (network error, 5xx, rate limit) and no verdict is cached,
// the request FAILS with: mis-partitioning is worse than a failed request,
// so there is no silent fingerprint fallback for a token that might belong to
// a user.
func requireAuth(gh *ghclient.Client, record identityRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, "unauthorized: missing Authorization header", http.StatusUnauthorized)
				return
			}
			ctx := ghclient.WithToken(r.Context(), token)

			// Trusted-app mode: a caller may assert a stable identity with a
			// GitHub App JWT in X-Mirror-Identity. We verify it against GitHub
			// (GET /app — unforgeable, since only the app's private key produces
			// a JWT GitHub accepts) and partition that caller by the app, NOT by
			// the token fingerprint. This lets a -party app whose
			// installation tokens rotate hourly share warm cache bucket,
			// while the Authorization token is still used for upstream fetches so
			// per-repo authorization is preserved. Callers without this header
			if idJWT := r.Header.Get("X-Mirror-Identity"); idJWT != "" {
				ident, err := gh.VerifyAppIdentity(ctx, idJWT)
				if err != nil {
					slog.Warn("verify app identity failed", "error", err)
					http.Error(w, "unauthorized: could not verify identity assertion", http.StatusUnauthorized)
					return
				}
				actorKey := fmt.Sprintf("app:%d", ident.ID)
				ctx = actor.WithActor(ctx, actorKey)
				if ident.Slug != "" {
					// The app slug is GitHub-verified (GET /app answered it):
					ctx = actor.WithName(ctx, ident.Slug)
				}
				record(ctx, actorKey, ident.Slug)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Resolve the credential's identity with GitHub up front (cached
			ident, err := gh.ResolveTokenIdentity(ctx)
			if err != nil {
				if errors.Is(err, ghclient.ErrBadCredential) {
					slog.Warn("resolve token identity: bad credential", "error", err)
					http.Error(w, "unauthorized: could not validate GitHub credential", http.StatusUnauthorized)
					return
				}
				slog.Warn("resolve token identity failed; refusing to guess a cache partition", "error", err)
				http.Error(w, "service unavailable: could not resolve the credential's GitHub identity (required for cache partitioning); please retry", http.StatusServiceUnavailable)
				return
			}
			actorKey := ghclient.Fingerprint(token)
			if ident.IsUser {
				actorKey = fmt.Sprintf("user:%d", ident.ID)
			}
			ctx = actor.WithActor(ctx, actorKey)
			if ident.IsUser && ident.Login != "" {
				// The login came from GitHub's own GET /user answer: carry it
				ctx = actor.WithName(ctx, ident.Login)
			}
			// Remember the actor->login mapping so the dashboard can group a
			record(ctx, actorKey, ident.Login)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// identityRecorder persists an actor->login mapping for the dashboard.
type identityRecorder func(ctx context.Context, actorFP, login string)

// newIdentityRecorder returns a recorder that upserts the actor->login mapping,
// debounced to at most per minute per actor so the hot request path does
// not write on every call.
func newIdentityRecorder(store *ghdata.Store) identityRecorder {
	var lastWrite sync.Map // actorFP -> time.Time
	return func(ctx context.Context, actorFP, login string) {
		if login == "" {
			return
		}
		if v, ok := lastWrite.Load(actorFP); ok {
			if t, ok := v.(time.Time); ok && time.Since(t) < time.Minute {
				return
			}
		}
		lastWrite.Store(actorFP, time.Now())
		if err := store.RecordActorIdentity(ctx, actorFP, login); err != nil {
			slog.Warn("record actor identity failed", "error", err)
		}
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" or
// "Authorization: token <token>" header, returning "" when absent.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	token = strings.TrimPrefix(token, "token ")
	return strings.TrimSpace(token)
}
