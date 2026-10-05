package authx

import (
	"reflect"
	"sort"
	"testing"

	"github.com/rakunlabs/ada/middleware/auth/identity"

	"github.com/rakunlabs/kutu/internal/service"
)

func tokenID(scopes ...service.TokenScope) *identity.Identity {
	return &identity.Identity{
		Subject:  "ci",
		Provider: service.TokenProvider,
		Claims:   map[string]any{service.TokenScopesClaim: scopes},
	}
}

func TestTokenReportMapsScopes(t *testing.T) {
	cases := []struct {
		name     string
		scopes   []service.TokenScope
		caps     []string
		patterns map[string][]string
	}{
		{
			name:   "everything read",
			scopes: []service.TokenScope{{Path: "**", Operations: []string{"read"}}},
			caps:   []string{service.CapRawRead, service.CapRegistryRead},
		},
		{
			name:   "registry repo push",
			scopes: []service.TokenScope{{Path: "registry/default/docker/**", Operations: []string{"read", "write"}}},
			caps:   []string{service.CapRegistryRead, service.CapRegistryWrite},
			patterns: map[string][]string{
				service.CapRegistryRead:  {"default/docker/**"},
				service.CapRegistryWrite: {"default/docker/**"},
			},
		},
		{
			name:   "raw delete maps to raw.write",
			scopes: []service.TokenScope{{Path: "raw/builds/**", Operations: []string{"delete"}}},
			caps:   []string{service.CapRawWrite},
			patterns: map[string][]string{
				service.CapRawWrite: {"builds/**"},
			},
		},
		{
			name:   "wildcard op on whole area is unrestricted",
			scopes: []service.TokenScope{{Path: "registry/**", Operations: []string{"*"}}},
			caps:   []string{service.CapRegistryDelete, service.CapRegistryRead, service.CapRegistryWrite},
		},
		{
			name:   "unknown area grants nothing",
			scopes: []service.TokenScope{{Path: "settings/**", Operations: []string{"*"}}},
			caps:   []string{},
		},
		{
			name:   "partial-segment glob stays literal",
			scopes: []service.TokenScope{{Path: "raw/my*", Operations: []string{"read"}}},
			caps:   []string{service.CapRawRead},
			patterns: map[string][]string{
				service.CapRawRead: {`my\*`},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := tokenReport(tokenID(tc.scopes...))
			got := append([]string(nil), rep.Capabilities...)
			sort.Strings(got)
			want := append([]string(nil), tc.caps...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("caps = %v, want %v", got, want)
			}
			if !reflect.DeepEqual(rep.Patterns, tc.patterns) {
				t.Fatalf("patterns = %v, want %v", rep.Patterns, tc.patterns)
			}
			for _, c := range rep.Capabilities {
				switch c {
				case service.CapRegistryAdmin, service.CapSettingsManage, service.CapUsersManage,
					service.CapPermissionsManage, service.CapTokensManage:
					t.Fatalf("token must never get %q", c)
				}
			}
		})
	}
}

func TestTokenPatternEnforcement(t *testing.T) {
	rep := tokenReport(tokenID(service.TokenScope{Path: "registry/team-a/*/**", Operations: []string{"read"}}))
	p := service.CapabilityPatterns(rep.Patterns)
	if !p.Allows(service.CapRegistryRead, "team-a/npm/lodash") {
		t.Fatal("expected team-a/npm/lodash to be allowed")
	}
	if p.Allows(service.CapRegistryRead, "team-b/npm/lodash") {
		t.Fatal("expected team-b to be denied")
	}
}
