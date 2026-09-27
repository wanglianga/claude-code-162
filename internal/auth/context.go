package auth

import (
	"context"
)

func contextWithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ClaimsKey, c)
}

// FromContext 取出当前登录人 Claims。
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ClaimsKey).(Claims)
	return c, ok
}
