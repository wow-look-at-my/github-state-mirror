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
