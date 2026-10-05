package npm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

func newRemoteWith(t *testing.T, repo *service.RegistryRepository) *Remote {
	t.Helper()
	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo.Name, repo.Type, repo.Kind = "npm-mirror", service.RegistryTypeNPM, service.RegistryKindRemote
	repo.Mount, repo.BasePath, repo.MutableTTL = "m", "npm", "1h"
	r, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	return r.(*Remote)
}

func packumentJSON(name, version, upstreamURL, extra string) string {
	bare := name[strings.LastIndex(name, "/")+1:]
	return `{"name":"` + name + `","dist-tags":{"latest":"` + version + `"},` + extra + `
		"versions":{"` + version + `":{"name":"` + name + `","version":"` + version + `","license":"MIT",
		"dist":{"tarball":"` + upstreamURL + `/` + name + `/-/` + bare + `-` + version + `.tgz"}}}}`
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/npm-mirror")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNPMRemote_ScopeRouting(t *testing.T) {
	pub := newFakeNPMUpstream()
	defer pub.Close()
	priv := newFakeNPMUpstream()
	defer priv.Close()

	pub.ServeJSON("/lodash", packumentJSON("lodash", "1.0.0", pub.URL(), ""))
	pub.ServeBytes("/lodash/-/lodash-1.0.0.tgz", []byte("PUB"))
	priv.ServeJSON("/@acme/widget", packumentJSON("@acme/widget", "2.0.0", priv.URL(), ""))
	priv.ServeBytes("/@acme/widget/-/widget-2.0.0.tgz", []byte("PRIV"))

	rr := newRemoteWith(t, &service.RegistryRepository{
		URL:       pub.URL(),
		Upstreams: []service.RegistryUpstream{{Prefix: "@acme", URL: priv.URL()}},
	})

	if w := get(t, rr, "/lodash"); w.Code != http.StatusOK {
		t.Fatalf("lodash packument %d %s", w.Code, w.Body.String())
	}
	w := get(t, rr, "/@acme%2fwidget")
	if w.Code != http.StatusOK {
		t.Fatalf("scoped packument %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/registries/default/npm-mirror/@acme/widget/-/widget-2.0.0.tgz") {
		t.Fatalf("tarball URL not rewritten: %s", w.Body.String())
	}
	if w := get(t, rr, "/@acme/widget/-/widget-2.0.0.tgz"); w.Body.String() != "PRIV" {
		t.Fatalf("scoped tarball = %q", w.Body.String())
	}
	if w := get(t, rr, "/lodash/-/lodash-1.0.0.tgz"); w.Body.String() != "PUB" {
		t.Fatalf("public tarball = %q", w.Body.String())
	}
	if pub.Hits() != 2 || priv.Hits() != 2 {
		t.Fatalf("hits pub=%d priv=%d, want 2/2", pub.Hits(), priv.Hits())
	}
	// "@acme" must not capture "@acmecorp/…".
	if w := get(t, rr, "/@acmecorp/x"); w.Code != http.StatusNotFound || priv.Hits() != 2 {
		t.Fatalf("@acmecorp routed to private upstream (code %d, hits %d)", w.Code, priv.Hits())
	}
}

func TestNPMClassifyRequest(t *testing.T) {
	l := newNPMLocal(t, true)
	cases := []struct {
		path    string
		name    string
		version string
		ok      bool
	}{
		{"/lodash", "lodash", "", true},
		{"/lodash/-/lodash-4.17.21.tgz", "lodash", "4.17.21", true},
		{"/@s/pkg/-/pkg-1.0.0-rc.1.tgz", "@s/pkg", "1.0.0-rc.1", true},
		{"/@s%2fpkg", "@s/pkg", "", true},
		{"/@s/pkg/-/s-pkg-2.0.0.tgz", "@s/pkg", "2.0.0", true},
		{"/-/v1/search?text=x", "", "", false},
		{"/-/whoami", "", "", false},
	}
	for _, c := range cases {
		ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, c.path, nil))
		if ok != c.ok || ref.Name != c.name || ref.Version != c.version {
			t.Errorf("%s: got %+v ok=%v", c.path, ref, ok)
		}
	}
}

func TestNPMLocal_ListDeleteInfo(t *testing.T) {
	l := newNPMLocal(t, true)
	publishVersion(t, l, "lodash", "1.0.0", []byte("a"))
	publishVersion(t, l, "lodash", "1.10.0", []byte("b"))
	publishVersion(t, l, "@s/pkg", "0.1.0", []byte("c"))

	ctx := context.Background()
	pkgs, err := l.ListPackages(ctx)
	if err != nil || len(pkgs) != 2 {
		t.Fatalf("ListPackages = %+v, %v", pkgs, err)
	}
	if pkgs[1].Name != "lodash" || strings.Join(pkgs[1].Versions, ",") != "1.0.0,1.10.0" {
		t.Fatalf("lodash summary %+v", pkgs[1])
	}

	info, err := l.ArtifactInfo(ctx, registry.ArtifactRef{Name: "lodash", Version: "1.0.0"})
	if err != nil || info.PublishedAt.IsZero() {
		t.Fatalf("ArtifactInfo = %+v, %v", info, err)
	}

	if err := l.DeleteVersion(ctx, "lodash", "1.10.0"); err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if err := l.DeleteVersion(ctx, "lodash", "9.9.9"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing delete err = %v", err)
	}
	var pkg Packument
	_ = json.Unmarshal(get(t, l, "/lodash").Body.Bytes(), &pkg)
	if _, ok := pkg.Versions["1.10.0"]; ok || pkg.DistTags["latest"] != "1.0.0" {
		t.Fatalf("after delete: tags=%v versions=%d", pkg.DistTags, len(pkg.Versions))
	}
}

func TestNPMLocal_PromoteVersion(t *testing.T) {
	src := newNPMLocal(t, true)
	dst := newNPMLocal(t, true)
	publishVersion(t, src, "@s/pkg", "1.0.0", []byte("TARBALL"))
	publishVersion(t, dst, "@s/pkg", "0.5.0", []byte("OLD"))

	if err := src.PromoteVersion(context.Background(), dst, "@s/pkg", "1.0.0"); err != nil {
		t.Fatalf("PromoteVersion: %v", err)
	}
	w := get(t, dst, "/@s/pkg")
	var pkg Packument
	_ = json.Unmarshal(w.Body.Bytes(), &pkg)
	if pkg.DistTags["latest"] != "1.0.0" {
		t.Fatalf("latest = %q", pkg.DistTags["latest"])
	}
	tb, _ := pkg.Versions["1.0.0"]["dist"].(map[string]any)["tarball"].(string)
	if !strings.HasPrefix(tb, "http://example.com/registries/default/npm-mirror/@s/pkg/-/") {
		t.Fatalf("tarball URL %q", tb)
	}
	if w := get(t, dst, "/@s/pkg/-/s-pkg-1.0.0.tgz"); w.Body.String() != "TARBALL" {
		t.Fatalf("promoted tarball %d %q", w.Code, w.Body.String())
	}
	if err := src.PromoteVersion(context.Background(), dst, "@s/pkg", "1.0.0"); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("re-promote err = %v", err)
	}
	if err := src.PromoteVersion(context.Background(), newRemoteWith(t, &service.RegistryRepository{URL: "http://x"}), "@s/pkg", "1.0.0"); err == nil {
		t.Fatal("promote into remote should fail")
	}
}

func TestNPMRemote_PrefetchInfoList(t *testing.T) {
	fu := newFakeNPMUpstream()
	defer fu.Close()
	fu.ServeJSON("/lodash", packumentJSON("lodash", "1.0.0", fu.URL(), `"time":{"1.0.0":"2020-01-02T03:04:05Z"},`))
	fu.ServeBytes("/lodash/-/lodash-1.0.0.tgz", []byte("TB"))
	rr := newRemote(t, fu.URL())
	ctx := context.Background()

	info, err := rr.ArtifactInfo(ctx, registry.ArtifactRef{Name: "lodash", Version: "1.0.0"})
	if err != nil || info.License != "MIT" || info.PublishedAt.Year() != 2020 {
		t.Fatalf("ArtifactInfo = %+v, %v", info, err)
	}
	if _, err := rr.ArtifactInfo(ctx, registry.ArtifactRef{Name: "lodash", Version: "2.0.0"}); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing version err = %v", err)
	}

	if err := rr.Prefetch(ctx, "lodash", ""); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	if rr.store.TarballSize("lodash", "lodash-1.0.0.tgz") != 2 {
		t.Fatal("tarball not cached by prefetch")
	}
	pkgs, err := rr.ListPackages(ctx)
	if err != nil || len(pkgs) != 1 || len(pkgs[0].Versions) != 1 {
		t.Fatalf("ListPackages = %+v, %v", pkgs, err)
	}
	if err := rr.Prefetch(ctx, "nope", ""); err == nil {
		t.Fatal("prefetch of missing package should fail")
	}
}

func postAudit(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"lodash":["1.0.0"]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNPMAudit(t *testing.T) {
	const bulk = "/-/npm/v1/security/advisories/bulk"
	var gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != bulk {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"lodash":[{"id":1}]}`)
	}))
	defer up.Close()

	l := newNPMLocal(t, true)
	if w := postAudit(t, l, bulk); w.Code != http.StatusOK || w.Body.String() != "{}" {
		t.Fatalf("local audit %d %s", w.Code, w.Body.String())
	}

	rr := newRemoteWith(t, &service.RegistryRepository{
		URL:  up.URL,
		Auth: &service.RegistryUpstreamAuth{Type: service.RegistryAuthBearer, Token: "tok"},
	})
	w := postAudit(t, rr, bulk)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":1`) {
		t.Fatalf("remote audit %d %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer tok" || gotBody != `{"lodash":["1.0.0"]}` {
		t.Fatalf("upstream saw auth=%q body=%q", gotAuth, gotBody)
	}

	resolver := &stubResolver{regs: map[string]registry.Registry{"l": l, "r": rr}}
	v := newVirtual(t, []string{"l", "r"}, resolver)
	if w := postAudit(t, v, bulk); !strings.Contains(w.Body.String(), `"id":1`) {
		t.Fatalf("virtual audit %d %s", w.Code, w.Body.String())
	}
	v2 := newVirtual(t, []string{"l"}, resolver)
	if w := postAudit(t, v2, "/-/npm/v1/security/audits/quick"); w.Code != http.StatusOK || w.Body.String() != "{}" {
		t.Fatalf("virtual local-only audit %d %s", w.Code, w.Body.String())
	}
}

func TestNPMVirtual_PackumentMergeTime(t *testing.T) {
	a := newNPMLocal(t, true)
	publishVersion(t, a, "lodash", "1.0.0", []byte("a"))
	b := newNPMLocal(t, true)
	publishVersion(t, b, "lodash", "1.0.0", []byte("b-dup"))
	publishVersion(t, b, "lodash", "2.0.0", []byte("b"))

	v := newVirtual(t, []string{"a", "b"}, &stubResolver{regs: map[string]registry.Registry{"a": a, "b": b}})
	var pkg Packument
	_ = json.Unmarshal(get(t, v, "/lodash").Body.Bytes(), &pkg)
	if len(pkg.Versions) != 2 || len(pkg.Time) != 2 {
		t.Fatalf("merged versions=%d time=%d", len(pkg.Versions), len(pkg.Time))
	}
	if pkg.DistTags["latest"] != "1.0.0" {
		t.Fatalf("first member should win dist-tags, got %v", pkg.DistTags)
	}
}
