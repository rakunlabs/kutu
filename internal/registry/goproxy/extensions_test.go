package goproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
)

func TestLocal_ListPackagesAndDeleteVersion(t *testing.T) {
	l := newLocal(t, true)
	mod := "github.com/Foo/bar"
	uploadInfo(t, l, mod, "v1.0.0")
	uploadInfo(t, l, mod, "v1.10.0")
	uploadInfo(t, l, mod, "v1.2.0")

	pkgs, err := l.ListPackages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].Name != mod {
		t.Fatalf("pkgs %+v", pkgs)
	}
	want := []string{"v1.0.0", "v1.2.0", "v1.10.0"}
	if len(pkgs[0].Versions) != 3 {
		t.Fatalf("versions %v", pkgs[0].Versions)
	}
	for i, v := range want {
		if pkgs[0].Versions[i] != v {
			t.Fatalf("versions %v, want %v", pkgs[0].Versions, want)
		}
	}

	if err := l.DeleteVersion(context.Background(), mod, "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(context.Background(), mod, "v1.2.0"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	w := httptest.NewRecorder()
	l.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+EncodeModulePath(mod)+"/@v/list", nil))
	if w.Body.String() != "v1.0.0\nv1.10.0\n" {
		t.Fatalf("list after delete %q", w.Body.String())
	}
}

func TestClassifyRequest(t *testing.T) {
	l := newLocal(t, false)
	cases := []struct {
		path    string
		ok      bool
		name    string
		version string
	}{
		{"/github.com/!azure/sdk/@v/v1.2.3.zip", true, "github.com/Azure/sdk", "v1.2.3"},
		{"/github.com/!azure/sdk/@v/v1.2.3.mod", true, "github.com/Azure/sdk", "v1.2.3"},
		{"/github.com/!azure/sdk/@v/v1.2.3.info", true, "github.com/Azure/sdk", "v1.2.3"},
		{"/github.com/!azure/sdk/@v/list", true, "github.com/Azure/sdk", ""},
		{"/github.com/!azure/sdk/@latest", true, "github.com/Azure/sdk", ""},
		{"/", false, "", ""},
		{"/garbage", false, "", ""},
	}
	for _, c := range cases {
		ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, c.path, nil))
		if ok != c.ok || ref.Name != c.name || ref.Version != c.version {
			t.Errorf("%s: got %+v ok=%v", c.path, ref, ok)
		}
	}
}

func TestLocal_ArtifactInfo(t *testing.T) {
	l := newLocal(t, true)
	mod := "github.com/foo/bar"
	ts := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := l.store.WriteInfo(mod, "v1.0.0", VersionInfo{Version: "v1.0.0", Time: ts}); err != nil {
		t.Fatal(err)
	}
	meta, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: mod, Version: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !meta.PublishedAt.Equal(ts) || meta.License != "" {
		t.Fatalf("meta %+v", meta)
	}
	if _, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: mod, Version: "v9.0.0"}); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestLocal_PromoteVersion(t *testing.T) {
	src := newLocal(t, true)
	dst := newLocal(t, true)
	mod := "github.com/foo/bar"
	uploadInfo(t, src, mod, "v1.0.0")
	uploadMod(t, src, mod, "v1.0.0", "module github.com/foo/bar\n")
	uploadZip(t, src, mod, "v1.0.0", []byte("ZIP"))

	if err := src.PromoteVersion(context.Background(), dst, mod, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"info", "mod", "zip"} {
		w := httptest.NewRecorder()
		dst.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+mod+"/@v/v1.0.0."+ext, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", ext, w.Code)
		}
	}
	w := httptest.NewRecorder()
	dst.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+mod+"/@v/list", nil))
	if w.Body.String() != "v1.0.0\n" {
		t.Fatalf("dst list %q", w.Body.String())
	}
	if err := src.PromoteVersion(context.Background(), dst, mod, "v2.0.0"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing version: %v", err)
	}
	if err := src.PromoteVersion(context.Background(), &Virtual{}, mod, "v1.0.0"); err == nil {
		t.Fatal("expected error for non-local dst")
	}
}

func TestRemote_PrefetchLatestAndListPackages(t *testing.T) {
	fu := newFakeUpstream()
	defer fu.Close()
	mod := "github.com/foo/bar"
	fu.Serve("/"+mod+"/@v/list", "text/plain", "v1.0.0\nv1.1.0\n")
	fu.Serve("/"+mod+"/@latest", "application/json", `{"Version":"v1.1.0","Time":"2024-01-02T00:00:00Z"}`)
	fu.Serve("/"+mod+"/@v/v1.1.0.info", "application/json", `{"Version":"v1.1.0","Time":"2024-01-02T00:00:00Z"}`)
	fu.Serve("/"+mod+"/@v/v1.1.0.mod", "text/plain", "module github.com/foo/bar\n")
	fu.Serve("/"+mod+"/@v/v1.1.0.zip", "application/zip", "ZIP")
	fu.Serve("/"+mod+"/@v/v1.0.0.info", "application/json", `{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`)
	fu.Serve("/"+mod+"/@v/v1.0.0.mod", "text/plain", "module github.com/foo/bar\n")
	fu.Serve("/"+mod+"/@v/v1.0.0.zip", "application/zip", "ZIP0")

	rr, _ := newRemote(t, fu.URL())
	ctx := context.Background()
	if err := rr.Prefetch(ctx, mod, ""); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"info", "mod", "zip"} {
		if _, err := rr.store.StatVersionFile(mod, "v1.1.0", ext); err != nil {
			t.Fatalf("v1.1.0.%s not cached: %v", ext, err)
		}
	}
	if err := rr.Prefetch(ctx, mod, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.store.StatVersionFile(mod, "v1.0.0", "zip"); err != nil {
		t.Fatalf("v1.0.0.zip not cached: %v", err)
	}
	if err := rr.Prefetch(ctx, "github.com/missing/mod", ""); err == nil {
		t.Fatal("expected error for missing module")
	}

	pkgs, err := rr.ListPackages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].Name != mod || len(pkgs[0].Versions) != 2 {
		t.Fatalf("pkgs %+v", pkgs)
	}

	meta, err := rr.ArtifactInfo(ctx, registry.ArtifactRef{Name: mod, Version: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if meta.PublishedAt.Year() != 2024 {
		t.Fatalf("meta %+v", meta)
	}
}

func TestRemote_ArtifactInfoFetchesInfo(t *testing.T) {
	fu := newFakeUpstream()
	defer fu.Close()
	mod := "github.com/foo/bar"
	fu.Serve("/"+mod+"/@v/v1.0.0.info", "application/json", `{"Version":"v1.0.0","Time":"2023-05-06T00:00:00Z"}`)
	rr, _ := newRemote(t, fu.URL())
	meta, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: mod, Version: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if meta.PublishedAt.Year() != 2023 {
		t.Fatalf("meta %+v", meta)
	}
	if _, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: mod, Version: "v9.9.9"}); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestVirtual_ListPackagesAndClassify(t *testing.T) {
	mod := "github.com/foo/bar"
	a := localWithVersion(t, mod, "v1.0.0", "a")
	b := localWithVersion(t, mod, "v2.0.0", "b")
	v := newVirtual(t, []string{"a", "b"}, &stubResolver{regs: map[string]registry.Registry{"a": a, "b": b}})
	pkgs, err := v.ListPackages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || len(pkgs[0].Versions) != 2 {
		t.Fatalf("pkgs %+v", pkgs)
	}
	ref, ok := v.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/"+mod+"/@v/v2.0.0.zip", nil))
	if !ok || ref.Name != mod || ref.Version != "v2.0.0" {
		t.Fatalf("classify %+v %v", ref, ok)
	}
}
