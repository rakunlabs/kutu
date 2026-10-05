package service

import (
	"context"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Capability keys are the leaf strings route handlers check. Each
// Permission record is a named bundle of these keys; external role/scope
// mappings point at bundles. This file is the single source of truth for
// the vocabulary — /api/v1/info ships KnownCapabilities so the UI can
// render self-describing permission editors.
//
// Path patterns (Permission.KeyPatterns) scope a grant:
//   - raw.*      → "<mount>/<path>"
//   - registry.* → "<namespace>/<repo>/<rest>"
const (
	CapRawRead  = "raw.read"
	CapRawWrite = "raw.write"

	CapRegistryRead   = "registry.read"
	CapRegistryWrite  = "registry.write"
	CapRegistryDelete = "registry.delete"
	CapRegistryAdmin  = "registry.admin"

	CapSettingsManage    = "settings.manage"
	CapTokensManage      = "tokens.manage"
	CapUsersManage       = "users.manage"
	CapPermissionsManage = "permissions.manage"
)

// Capability describes a capability key for UI discovery.
type Capability struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// KnownCapabilities is the canonical capability list, in UI order.
var KnownCapabilities = []Capability{
	{CapRawRead, "Raw Files Read", "Browse and download files from raw mounts"},
	{CapRawWrite, "Raw Files Write", "Upload, delete, rename, copy and move files on raw mounts"},
	{CapRegistryRead, "Registry Read", "Browse repositories and pull artifacts"},
	{CapRegistryWrite, "Registry Write", "Publish and push artifacts to local registries"},
	{CapRegistryDelete, "Registry Delete", "Remove tags, versions and manifests; purge caches"},
	{CapRegistryAdmin, "Registry Admin", "Manage namespaces, repositories and listeners; run garbage collection"},
	{CapSettingsManage, "Settings Management", "Raw mounts, file serving, encryption key and authentication settings"},
	{CapTokensManage, "Token Management", "Create, edit and revoke API access tokens"},
	{CapUsersManage, "User Management", "Create, edit, delete and kick users"},
	{CapPermissionsManage, "Permission Management", "Create, edit, delete permissions and assign them to users"},
}

// KnownCapabilityKeys returns the capability key strings. Superadmins are
// granted the whole list.
func KnownCapabilityKeys() []string {
	keys := make([]string, len(KnownCapabilities))
	for i, c := range KnownCapabilities {
		keys[i] = c.Key
	}
	return keys
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

// CapabilityPatterns maps a capability key to its path-glob restrictions.
// A key absent from the map (or with an empty slice) is unrestricted.
type CapabilityPatterns map[string][]string

// Allows reports whether path is permitted for key. An empty path is an
// unscoped check and always allowed. Malformed patterns never match.
func (cp CapabilityPatterns) Allows(key, path string) bool {
	if cp == nil {
		return true
	}
	pats, ok := cp[key]
	if !ok || len(pats) == 0 {
		return true
	}
	if path == "" {
		return true
	}
	clean := strings.TrimLeft(path, "/")
	for _, pat := range pats {
		ok, err := doublestar.Match(pat, clean)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// AllowsAncestor is Allows that also accepts directories leading to a
// permitted path, so a user granted `builds/team-a/**` can still list
// `builds` on the way down.
func (cp CapabilityPatterns) AllowsAncestor(key, path string) bool {
	if cp.Allows(key, path) {
		return true
	}
	pats := cp[key]
	if len(pats) == 0 {
		return true
	}
	pathSegs := splitSegments(strings.Trim(path, "/"))
	for _, pat := range pats {
		if isAncestorOfPattern(pathSegs, pat) {
			return true
		}
	}
	return false
}

func splitSegments(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func isAncestorOfPattern(pathSegs []string, pattern string) bool {
	patSegs := strings.Split(pattern, "/")
	pi := 0
	for _, seg := range pathSegs {
		if pi >= len(patSegs) {
			return false
		}
		ps := patSegs[pi]
		if ps == "**" {
			return true
		}
		ok, err := doublestar.Match(ps, seg)
		if err != nil || !ok {
			return false
		}
		pi++
	}
	return pi < len(patSegs)
}

type (
	capabilitiesCtxKey       struct{}
	capabilityPatternsCtxKey struct{}
)

// WithCapabilities attaches a resolved capability set to ctx.
func WithCapabilities(ctx context.Context, keys []string) context.Context {
	return context.WithValue(ctx, capabilitiesCtxKey{}, Capabilities(keys))
}

// WithAllCapabilities attaches the full capability set to ctx. Only for
// internal callers and tests; requests get their set from the resolver.
func WithAllCapabilities(ctx context.Context) context.Context {
	return WithCapabilities(ctx, KnownCapabilityKeys())
}

// CapabilitiesFromContext returns the capability set attached via
// WithCapabilities, or an empty set.
func CapabilitiesFromContext(ctx context.Context) Capabilities {
	v, _ := ctx.Value(capabilitiesCtxKey{}).(Capabilities)
	return v
}

// WithCapabilityPatterns attaches the per-key path patterns to ctx. nil
// means unrestricted.
func WithCapabilityPatterns(ctx context.Context, patterns map[string][]string) context.Context {
	if patterns == nil {
		return ctx
	}
	return context.WithValue(ctx, capabilityPatternsCtxKey{}, CapabilityPatterns(patterns))
}

// CapabilityPatternsFromContext returns the per-key patterns, nil when
// unrestricted.
func CapabilityPatternsFromContext(ctx context.Context) CapabilityPatterns {
	v, _ := ctx.Value(capabilityPatternsCtxKey{}).(CapabilityPatterns)
	return v
}
