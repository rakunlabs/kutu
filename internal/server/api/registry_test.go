package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/ada"
	"github.com/rakunlabs/query"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/service"
)

// memStore is a no-op in-memory service.Storage for tests. Every method
// returns empty data; the routing tests below only need Handle to wire
// without panicking and the registry feature gate to read flags.
type memStore struct {
	meta map[string][]byte
}

func newMemStore() *memStore { return &memStore{meta: map[string][]byte{}} }

// RegistryStore
func (m *memStore) LoadRegistryTree(context.Context) (*service.RegistrySettings, error) {
	return &service.RegistrySettings{}, nil
}
func (m *memStore) ListNamespaces(context.Context) ([]service.RegistryNamespace, error) {
	return nil, nil
}
func (m *memStore) GetNamespace(context.Context, string) (*service.RegistryNamespace, error) {
	return nil, service.ErrNotFound
}
func (m *memStore) CountNamespaces(context.Context) (int, error) { return 0, nil }
func (m *memStore) CreateNamespace(context.Context, *service.RegistryNamespace) error {
	return nil
}
func (m *memStore) UpdateNamespace(context.Context, *service.RegistryNamespace) error {
	return nil
}
func (m *memStore) DeleteNamespace(context.Context, string) error { return nil }
func (m *memStore) ListRepositories(context.Context, string, *query.Query) ([]service.RegistryRepositoryRow, error) {
	return nil, nil
}
func (m *memStore) GetRepository(context.Context, string, string) (*service.RegistryRepository, error) {
	return nil, service.ErrNotFound
}
func (m *memStore) CreateRepository(context.Context, string, *service.RegistryRepository) error {
	return nil
}
func (m *memStore) UpdateRepository(context.Context, string, *service.RegistryRepository) error {
	return nil
}
func (m *memStore) DeleteRepository(context.Context, string, string) error { return nil }

// RawMountStore
func (m *memStore) ListRawMounts(context.Context, *query.Query) ([]service.RawMountEntry, error) {
	return nil, nil
}
func (m *memStore) GetRawMount(context.Context, string) (*service.RawMountEntry, error) {
	return nil, service.ErrNotFound
}
func (m *memStore) CreateRawMount(context.Context, *service.RawMountEntry) error { return nil }
func (m *memStore) UpdateRawMount(context.Context, *service.RawMountEntry) error { return nil }
func (m *memStore) DeleteRawMount(context.Context, string) error                 { return nil }

// ProxyStore
func (m *memStore) ListProxyListeners(context.Context, *query.Query) ([]service.ProxyListener, error) {
	return nil, nil
}
func (m *memStore) GetProxyListener(context.Context, string) (*service.ProxyListener, error) {
	return nil, service.ErrNotFound
}
func (m *memStore) CreateProxyListener(context.Context, *service.ProxyListener) error { return nil }
func (m *memStore) UpdateProxyListener(context.Context, *service.ProxyListener) error { return nil }
func (m *memStore) DeleteProxyListener(context.Context, string) error                 { return nil }
func (m *memStore) ListProxyServers(context.Context, *query.Query) ([]service.ProxyServer, error) {
	return nil, nil
}
func (m *memStore) GetProxyServer(context.Context, string) (*service.ProxyServer, error) {
	return nil, service.ErrNotFound
}
func (m *memStore) CreateProxyServer(context.Context, *service.ProxyServer) error { return nil }
func (m *memStore) UpdateProxyServer(context.Context, *service.ProxyServer) error { return nil }
func (m *memStore) DeleteProxyServer(context.Context, string) error               { return nil }

// HookStore
func (m *memStore) ListHooks(context.Context) ([]hook.Hook, error)  { return nil, nil }
func (m *memStore) ReplaceHooks(context.Context, []hook.Hook) error { return nil }

// MetaStore
func (m *memStore) GetMeta(_ context.Context, key string, dest any) (bool, error) {
	raw, ok := m.meta[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, dest)
}

func (m *memStore) SetMeta(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.meta[key] = raw
	return nil
}

func TestHandleRegistersRoutesWithoutGreedyPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	rh := NewRawHandler(nil, ctx, nil)

	if err := Handle(ada.NewMux(), service.New(newMemStore()), Info{}, rh, nil, nil, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestRegistryAdminActionRoutesDoNotFallThroughToSPAFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	rh := NewRawHandler(nil, ctx, nil)

	server := ada.New()
	if err := Handle(server.Mux, service.New(newMemStore()), Info{}, rh, nil, nil, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	server.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>pika</html>"))
	}))

	cases := []struct {
		name   string
		method string
		path   string
		cap    string
	}{
		{"test upstream", http.MethodPost, "/api/v1/registries/npm/default/npm/test-upstream", service.CapRegistryAdmin},
		{"stats", http.MethodGet, "/api/v1/registries/npm/default/npm/stats", service.CapRegistryRead},
		{"purge", http.MethodPost, "/api/v1/registries/npm/default/npm/purge", service.CapRegistryAdmin},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(service.WithCapabilities(req.Context(), []string{tc.cap}))
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)

			if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<html>") {
				t.Fatalf("route fell through to SPA fallback: status=%d body=%q", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
				t.Fatalf("route returned HTML fallback content-type %q", ct)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d (registry manager intentionally nil)", rec.Code, http.StatusNotFound)
			}
		})
	}
}

func TestRegistryWildcardRoutesCaptureNestedNames(t *testing.T) {
	m := ada.NewMux()

	var gotPackageName, gotModulePath string
	m.GET("/api/v1/registries/go/{ns}/{repo}/modules", m.Wrap(func(c *ada.Context) error {
		return c.SendNoContent()
	}))
	m.GET("/api/v1/registries/go/{ns}/{repo}/packages/*", m.Wrap(func(c *ada.Context) error {
		gotPackageName = c.Request.PathValue("*")
		return c.SendNoContent()
	}))
	m.GET("/api/v1/registries/go/{ns}/{repo}/modules/*", m.Wrap(func(c *ada.Context) error {
		gotModulePath = c.Request.PathValue("*")
		return c.SendNoContent()
	}))

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/registries/go/default/mirror/packages/example.com/acme/foo", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("package detail route status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if gotPackageName != "example.com/acme/foo" {
		t.Fatalf("package detail wildcard capture = %q, want example.com/acme/foo", gotPackageName)
	}

	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/registries/go/default/mirror/modules/example.com/acme/foo/versions/v1.2.3/gomod", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("go.mod route status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if gotModulePath != "example.com/acme/foo/versions/v1.2.3/gomod" {
		t.Fatalf("go.mod wildcard capture = %q", gotModulePath)
	}
}

func TestParseGoModuleGoModPath(t *testing.T) {
	tests := []struct {
		path        string
		wantName    string
		wantVersion string
		wantOK      bool
	}{
		{
			path:        "example.com/acme/foo/versions/v1.2.3/gomod",
			wantName:    "example.com/acme/foo",
			wantVersion: "v1.2.3",
			wantOK:      true,
		},
		{
			path:        "/example.com/acme/versions/lib/versions/v0.1.0/gomod",
			wantName:    "example.com/acme/versions/lib",
			wantVersion: "v0.1.0",
			wantOK:      true,
		},
		{path: "example.com/mod/versions/v1.0.0/gomod/extra"},
		{path: "example.com/mod/versions//gomod"},
		{path: "example.com/mod/gomod"},
	}

	for _, tt := range tests {
		gotName, gotVersion, gotOK := parseGoModuleGoModPath(tt.path)
		if gotName != tt.wantName || gotVersion != tt.wantVersion || gotOK != tt.wantOK {
			t.Fatalf("parseGoModuleGoModPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.path, gotName, gotVersion, gotOK, tt.wantName, tt.wantVersion, tt.wantOK)
		}
	}
}

func TestRegistrySettingsForResponseRedactsSecretsForReadOnly(t *testing.T) {
	rs := &service.RegistrySettings{Namespaces: []service.RegistryNamespace{{
		Name: "default",
		Repositories: []service.RegistryRepository{
			{
				Name: "mirror", Type: service.RegistryTypeGo, Kind: service.RegistryKindRemote,
				URL: "https://proxy.golang.org", Mount: "m", BasePath: "go-cache",
				Auth: &service.RegistryUpstreamAuth{
					Type:     service.RegistryAuthHeader,
					Username: "user-visible",
					Password: "p",
					Token:    "raw://secrets/token",
					Header:   "X-Api-Key",
					Value:    "v",
				},
			},
			{
				Name: "local", Type: service.RegistryTypeDocker, Kind: service.RegistryKindLocal,
				Mount: "m", BasePath: "docker", AllowPush: true,
				Policy: &service.RegistryPolicy{
					ImmutableTags: []string{"prod"},
					Retention: &service.RegistryRetentionPolicy{
						GCMinAgeSeconds:              7200,
						AbandonedUploadMaxAgeSeconds: 300,
					},
				},
			},
		},
	}}}

	got := registrySettingsForResponse(rs, false)
	auth := got.Namespaces[0].Repositories[0].Auth
	if auth.Password != redactedRegistrySecret || auth.Token != redactedRegistrySecret || auth.Value != redactedRegistrySecret {
		t.Fatalf("secrets were not redacted: %+v", auth)
	}
	if auth.Username != "user-visible" || auth.Header != "X-Api-Key" {
		t.Fatalf("non-secret auth fields changed: %+v", auth)
	}
	if rs.Namespaces[0].Repositories[0].Auth.Token != "raw://secrets/token" {
		t.Fatalf("redaction mutated source settings")
	}
	got.Namespaces[0].Repositories[1].Policy.ImmutableTags[0] = "changed"
	if rs.Namespaces[0].Repositories[1].Policy.ImmutableTags[0] != "prod" {
		t.Fatalf("policy clone mutated source settings")
	}
}

func TestRegistrySettingsForResponseIncludesSecretsForAdmin(t *testing.T) {
	rs := &service.RegistrySettings{Namespaces: []service.RegistryNamespace{{
		Name: "default",
		Repositories: []service.RegistryRepository{{
			Name: "mirror", Type: service.RegistryTypeGo, Kind: service.RegistryKindRemote,
			URL: "https://proxy.golang.org", Mount: "m", BasePath: "go-cache",
			Auth: &service.RegistryUpstreamAuth{Type: service.RegistryAuthBearer, Token: "tk_secret"},
		}},
	}}}

	got := registrySettingsForResponse(rs, true)
	if got.Namespaces[0].Repositories[0].Auth.Token != "tk_secret" {
		t.Fatalf("admin response should include secret, got %+v", got.Namespaces[0].Repositories[0].Auth)
	}
}
