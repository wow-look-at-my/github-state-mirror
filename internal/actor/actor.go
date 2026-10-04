package actor

import "context"

type contextKey struct{}

var actorCtxKey = contextKey{}

type nameContextKey struct{}

var nameCtxKey = nameContextKey{}

// WithActor stores the actor's cache partition key.
func WithActor(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, actorCtxKey, key)
}

// FromContext returns the actor from context, or "" if absent.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorCtxKey).(string); ok {
		return v
	}
	return ""
}

// WithName stores a display-only name; see docs/ghclient.md.
func WithName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, nameCtxKey, name)
}

// NameFromContext returns the actor's verified display name from context, or
// "" if none was set.
func NameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(nameCtxKey).(string); ok {
		return v
	}
	return ""
}

// see docs/ghclient.md.
func Short(a string) string {
	if len(a) > 12 && isHex(a) {
		return a[:12]
	}
	return a
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
