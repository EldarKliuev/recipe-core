// Package authctx carries the authenticated user id through the request
// context. It lives in platform so that feature packages can read the caller
// without importing internal/auth.
package authctx

import (
	"context"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

type ctxKey struct{}

func With(ctx context.Context, userID uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKey{}, userID)
}

// UserID returns the caller id. It fails only if the route was mounted without
// the auth middleware, which is a programming error rather than a client one.
func UserID(ctx context.Context) (uuid.UUID, error) {
	id, ok := ctx.Value(ctxKey{}).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, httpx.ErrUnauthorized
	}
	return id, nil
}
