package actor

import "context"

type contextKey struct{}

var actorCtxKey = contextKey{}

// WithActor returns a child context carrying the given actor (cache partition
// key: "user:<id>", a token fingerprint, "app:<id>", or "app-installation:<id>").
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

// Short abbreviates an actor for display and logs. Only opaque hex token
// fingerprints (longer than 12 chars) are shortened, to their first 12 hex
// chars; structured actors — "user:<id>", "app:<id>", "app-installation:<id>"
// — are short and meaningful already, and truncating them would drop
// significant digits, so they are returned whole.
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
