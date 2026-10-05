package service

import "context"

// Actor is the authenticated user (or API token name) that performed a
// request; empty for anonymous registry reads. The storage layer records
// it in the updated_by column and the registry handlers stamp it on hook
// events so changes can be attributed.
type actorCtxKey struct{}

// WithActor attaches the request actor to ctx.
func WithActor(ctx context.Context, user string) context.Context {
	if user == "" {
		return ctx
	}
	return context.WithValue(ctx, actorCtxKey{}, user)
}

// ActorFromContext returns the request actor, or "" when none was set.
func ActorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(actorCtxKey{}).(string)
	return v
}
