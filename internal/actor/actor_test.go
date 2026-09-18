package actor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFromContext_Empty(t *testing.T) {
	assert.Equal(t, "", FromContext(context.Background()))
}

func TestWithActor_RoundTrip(t *testing.T) {
	ctx := WithActor(context.Background(), "octocat")
	assert.Equal(t, "octocat", FromContext(ctx))
}

func TestWithActor_Override(t *testing.T) {
	ctx := WithActor(context.Background(), "first")
	ctx = WithActor(ctx, "second")
	assert.Equal(t, "second", FromContext(ctx))
}

func TestNameFromContext_Empty(t *testing.T) {
	assert.Equal(t, "", NameFromContext(context.Background()))
}

func TestWithName_RoundTrip(t *testing.T) {
	ctx := WithName(context.Background(), "octocat")
	assert.Equal(t, "octocat", NameFromContext(ctx))
}

func TestWithName_Override(t *testing.T) {
	ctx := WithName(context.Background(), "first")
	ctx = WithName(ctx, "second")
	assert.Equal(t, "second", NameFromContext(ctx))
}

func TestWithName_IndependentOfActor(t *testing.T) {
	ctx := WithActor(context.Background(), "user:42")
	assert.Equal(t, "", NameFromContext(ctx), "an actor alone carries no name")

	ctx = WithName(ctx, "octocat")
	assert.Equal(t, "user:42", FromContext(ctx), "setting a name must not disturb the actor")
	assert.Equal(t, "octocat", NameFromContext(ctx))
}
