package authx

import (
	"strings"

	"github.com/rakunlabs/ada/middleware/auth/identity"

	"github.com/rakunlabs/kutu/internal/service"
)

// Token scope operations, as stored on service.TokenScope.
const (
	tokenOpRead   = "read"
	tokenOpWrite  = "write"
	tokenOpDelete = "delete"
	tokenOpAny    = "*"
)

// tokenScopeArea is one capability family a token scope path can land in.
// prefix is the scope-path namespace ("raw", "registry"); the remainder of
// the path is the capability pattern, in the same dialect bundles use.
type tokenScopeArea struct {
	prefix string
	ops    map[string]string // token op -> capability key
}

var tokenScopeAreas = []tokenScopeArea{
	{prefix: "raw", ops: map[string]string{
		tokenOpRead:   service.CapRawRead,
		tokenOpWrite:  service.CapRawWrite,
		tokenOpDelete: service.CapRawWrite,
	}},
	{prefix: "registry", ops: map[string]string{
		tokenOpRead:   service.CapRegistryRead,
		tokenOpWrite:  service.CapRegistryWrite,
		tokenOpDelete: service.CapRegistryDelete,
	}},
}

// tokenReport projects an API token's scopes onto the capability
// vocabulary so the same route guards serve browser sessions and token
// callers. Only data-plane capabilities (raw.*, registry.read/write/delete)
// can be reached; a token never gets an admin or manage capability.
//
//	"raw/<mount>/<glob>"        → raw.read / raw.write on "<mount>/<glob>"
//	"registry/<ns>/<repo>/..."  → registry.read / write / delete on "<ns>/<repo>/..."
//	"**" or "*"                 → every data-plane capability, unrestricted
func tokenReport(id *identity.Identity) *EffectiveReport {
	scopes := service.TokenScopesFromIdentity(id)

	rep := &EffectiveReport{
		Username:     id.Subject,
		Roles:        []string{},
		Scopes:       id.Scopes,
		Capabilities: []string{},
		Sources:      []CapSource{},
		Denied:       []string{},
	}
	if rep.Scopes == nil {
		rep.Scopes = []string{}
	}

	unrestricted := map[string]bool{}
	patterns := map[string][]string{}
	grant := func(capKey, pattern string) {
		if !service.Capabilities(rep.Capabilities).Has(capKey) {
			rep.Capabilities = append(rep.Capabilities, capKey)
			rep.Sources = append(rep.Sources, CapSource{Capability: capKey, Kind: "token_scope"})
		}
		if pattern == "" {
			unrestricted[capKey] = true
			return
		}
		for _, p := range patterns[capKey] {
			if p == pattern {
				return
			}
		}
		patterns[capKey] = append(patterns[capKey], pattern)
	}

	for _, sc := range scopes {
		path := strings.Trim(sc.Path, "/")
		for _, area := range tokenScopeAreas {
			pattern, ok := scopePatternFor(path, area.prefix)
			if !ok {
				continue
			}
			for op, capKey := range area.ops {
				if scopeHasAnyOp(sc, []string{op}) {
					grant(capKey, pattern)
				}
			}
		}
	}

	for k := range unrestricted {
		delete(patterns, k)
	}
	if len(patterns) > 0 {
		rep.Patterns = patterns
	}
	return rep
}

// scopePatternFor returns the capability pattern a token scope path grants
// within area prefix. "" means unrestricted. ok is false when the path
// does not reach the area at all.
func scopePatternFor(path, prefix string) (string, bool) {
	if path == "**" || path == "*" {
		return "", true
	}
	head, rest, _ := strings.Cut(path, "/")
	switch head {
	case "**":
		return "", true
	case "*", prefix:
	default:
		return "", false
	}
	if rest == "" {
		return "", false
	}
	if rest == "**" {
		return "", true
	}
	return normalizeScopePath(rest), true
}

func scopeHasAnyOp(scope service.TokenScope, want []string) bool {
	for _, have := range scope.Operations {
		if have == tokenOpAny {
			return true
		}
		for _, w := range want {
			if have == w {
				return true
			}
		}
	}
	return false
}

// normalizeScopePath translates a token scope path into the equivalent
// doublestar pattern. The token matcher compares segments literally
// unless the whole segment is "*" or "**", so other glob metacharacters
// are escaped — a scope like "my*" must not widen into a prefix glob.
func normalizeScopePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "*" || segment == "**" {
			continue
		}
		segments[i] = escapeGlobMeta(segment)
	}
	return strings.Join(segments, "/")
}

func escapeGlobMeta(segment string) string {
	if !strings.ContainsAny(segment, `*?[]{}\`) {
		return segment
	}
	var b strings.Builder
	b.Grow(len(segment) * 2)
	for _, r := range segment {
		if strings.ContainsRune(`*?[]{}\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
