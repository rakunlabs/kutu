package service

import "context"

// Capability keys gate the feature surface. kutu runs without
// authentication, so in practice the request context always carries
// the full capability set (see WithAllCapabilities). The constants are
// kept so the registry/raw handlers can express intent and so token
// scopes round-trip with pika.
const (
	CapRawRead  = "raw.read"
	CapRawWrite = "raw.write"

	CapRegistryRead   = "registry.read"
	CapRegistryWrite  = "registry.write"
	CapRegistryDelete = "registry.delete"
	CapRegistryAdmin  = "registry.admin"
)

// AllCapabilities is the full capability vocabulary. Because kutu does
// not enforce auth, the server plants this set on every request so the
// capability checks inherited from pika always pass.
var AllCapabilities = []string{
	CapRawRead, CapRawWrite,
	CapRegistryRead, CapRegistryWrite, CapRegistryDelete, CapRegistryAdmin,
}

// Capabilities is the resolved capability-key set for a request.
type Capabilities []string

// Has reports whether the set contains the given capability key.
func (c Capabilities) Has(key string) bool {
	for _, k := range c {
		if k == key {
			return true
		}
	}
	return false
}

type capabilitiesCtxKey struct{}

// WithCapabilities attaches a resolved capability set to ctx.
func WithCapabilities(ctx context.Context, keys []string) context.Context {
	return context.WithValue(ctx, capabilitiesCtxKey{}, Capabilities(keys))
}

// WithAllCapabilities attaches the full capability set to ctx. Used by
// the server middleware since kutu does not authenticate requests.
func WithAllCapabilities(ctx context.Context) context.Context {
	return WithCapabilities(ctx, AllCapabilities)
}

// CapabilitiesFromContext returns the capability set attached via
// WithCapabilities. Returns an empty slice when nothing is attached.
func CapabilitiesFromContext(ctx context.Context) Capabilities {
	v, _ := ctx.Value(capabilitiesCtxKey{}).(Capabilities)
	return v
}
