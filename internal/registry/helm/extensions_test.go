package helm

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

func publish(t *testing.T, l *Local, name, version string) []byte {
	t.Helper()
	body := buildChartTarball(t, name, version, "d", "")
	req := httptest.NewRequest(http.MethodPut, "/"+name+"-"+version+".tgz", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	l.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish %s@%s: %d %s", name, version, rec.Code, rec.Body.String())
	}
	return body
}

func getIndex(t *testing.T, reg registry.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index: %d", rec.Code)
	}
	return rec.Body.String()
}

func TestLocal_ListPackagesDeleteVersionInfo(t *testing.T) {
	l := newLocal(t, true)
	publish(t, l, "app", "1.0.0")
	publish(t, l, "app", "1.10.0")
	publish(t, l, "app", "1.2.0")
	publish(t, l, "other", "0.1.0")
	ctx := context.Background()

	pkgs, err := l.ListPackages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || pkgs[0].Name != "app" || strings.Join(pkgs[0].Versions, ",") != "1.0.0,1.2.0,1.10.0" {
		t.Fatalf("pkgs %+v", pkgs)
	}

	meta, err := l.ArtifactInfo(ctx, registry.ArtifactRef{Name: "app", Version: "1.2.0"})
	if err != nil || meta.PublishedAt.IsZero() {
		t.Fatalf("info %+v %v", meta, err)
	}
	if _, err := l.ArtifactInfo(ctx, registry.ArtifactRef{Name: "app", Version: "9.9.9"}); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing info: %v", err)
	}

	if err := l.DeleteVersion(ctx, "app", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(ctx, "app", "1.2.0"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	idx := getIndex(t, l)
	if strings.Contains(idx, "app-1.2.0.tgz") || !strings.Contains(idx, "app-1.10.0.tgz") {
		t.Fatalf("index after delete:\n%s", idx)
	}
}

func TestClassifyRequest(t *testing.T) {
	l := newLocal(t, false)
	cases := []struct {
		path, name, version string
		ok                  bool
	}{
		{"/my-chart-1.2.3.tgz", "my-chart", "1.2.3", true},
		{"/index.yaml", "", "", false},
		{"/api/charts", "", "", false},
		{"/api/charts/x-1.0.0.tgz", "", "", false},
	}
	for _, c := range cases {
		ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, c.path, nil))
		if ok != c.ok || ref.Name != c.name || ref.Version != c.version {
			t.Errorf("%s: %+v %v", c.path, ref, ok)
		}
	}
}

func TestLocal_PromoteVersion(t *testing.T) {
	src := newLocal(t, true)
	dst := newLocal(t, true)
	body := publish(t, src, "app", "1.0.0")
	ctx := context.Background()

	if err := src.PromoteVersion(ctx, dst, "app", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(getIndex(t, dst), "app-1.0.0.tgz") {
		t.Fatal("dst index missing promoted chart")
	}
	rec := httptest.NewRecorder()
	dst.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app-1.0.0.tgz", nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("dst tarball: %d", rec.Code)
	}
	if err := src.PromoteVersion(ctx, dst, "app", "2.0.0"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := src.PromoteVersion(ctx, &Virtual{}, "app", "1.0.0"); err == nil {
		t.Fatal("expected error for non-local dst")
	}
}

func newRemote(t *testing.T, url string) *Remote {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{
		Name: "helm-remote", Type: service.RegistryTypeHelm, Kind: service.RegistryKindRemote,
		Mount: "m", BasePath: "helm", URL: url, MutableTTL: "1h",
	}
	r, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Remote)
}

func TestRemote_PrefetchInfoListPackages(t *testing.T) {
	v1 := buildChartTarball(t, "app", "1.0.0", "", "")
	v2 := buildChartTarball(t, "app", "1.1.0", "", "")
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`apiVersion: v1
entries:
  app:
  - name: app
    version: 1.1.0
    created: "2024-02-03T04:05:06Z"
    urls: [charts/app-1.1.0.tgz]
  - name: app
    version: 1.0.0
    created: "2023-01-01T00:00:00.123456Z"
    urls: [app-1.0.0.tgz]
`))
	})
	mux.HandleFunc("/charts/app-1.1.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(v2) })
	mux.HandleFunc("/app-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(v1) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	rr := newRemote(t, srv.URL)
	ctx := context.Background()

	meta, err := rr.ArtifactInfo(ctx, registry.ArtifactRef{Name: "app", Version: "1.1.0"})
	if err != nil || meta.PublishedAt.Year() != 2024 {
		t.Fatalf("info %+v %v", meta, err)
	}
	meta, err = rr.ArtifactInfo(ctx, registry.ArtifactRef{Name: "app", Version: "1.0.0"})
	if err != nil || meta.PublishedAt.Year() != 2023 {
		t.Fatalf("info %+v %v", meta, err)
	}
	if _, err := rr.ArtifactInfo(ctx, registry.ArtifactRef{Name: "app", Version: "3.0.0"}); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing: %v", err)
	}

	if err := rr.Prefetch(ctx, "app", ""); err != nil {
		t.Fatal(err)
	}
	if err := rr.Prefetch(ctx, "app", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := rr.Prefetch(ctx, "ghost", ""); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("ghost: %v", err)
	}
	pkgs, err := rr.ListPackages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || strings.Join(pkgs[0].Versions, ",") != "1.0.0,1.1.0" {
		t.Fatalf("pkgs %+v", pkgs)
	}

	before := hits.Load()
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app-1.1.0.tgz", nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), v2) {
		t.Fatalf("cached tarball: %d", rec.Code)
	}
	if hits.Load() != before {
		t.Fatal("prefetched tarball hit upstream")
	}
}

type stubResolver map[string]registry.Registry

func (s stubResolver) Lookup(_, repo string) (registry.Registry, bool) {
	r, ok := s[repo]
	return r, ok
}

func TestVirtual_ListPackagesClassify(t *testing.T) {
	a := newLocal(t, true)
	b := newLocal(t, true)
	publish(t, a, "app", "1.0.0")
	publish(t, b, "app", "2.0.0")
	publish(t, b, "zed", "0.1.0")
	r, err := NewVirtualFactory(stubResolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default",
		&service.RegistryRepository{Name: "v", Members: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	v := r.(*Virtual)
	pkgs, err := v.ListPackages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || strings.Join(pkgs[0].Versions, ",") != "1.0.0,2.0.0" {
		t.Fatalf("pkgs %+v", pkgs)
	}
	if ref, ok := v.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/zed-0.1.0.tgz", nil)); !ok || ref.Name != "zed" {
		t.Fatalf("classify %+v", ref)
	}
}
