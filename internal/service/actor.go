package service

import "context"

// Actor is the identity that performed a request, taken from the
// optional X-User request header. It is purely advisory (kutu has no
// authentication) and may be empty. The storage layer records it in the
// updated_by column and the registry handlers stamp it on hook events so
// changes can be attributed.
type actorCtxKey struct{}

// WithActor attaches the request actor (X-User) to ctx.
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
