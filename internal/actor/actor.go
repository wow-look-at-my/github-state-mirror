package actor

import "context"

type contextKey struct{}

var actorCtxKey = contextKey{}

// WithActor returns a child context carrying the given actor (GitHub login).
func WithActor(ctx context.Context, login string) context.Context {
	return context.WithValue(ctx, actorCtxKey, login)
}

// FromContext returns the actor from context, or "" if absent.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorCtxKey).(string); ok {
		return v
	}
	return ""
}
