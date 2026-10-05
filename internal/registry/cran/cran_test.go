package cran

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
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

func description(name, version, extra string) string {
	return "Package: " + name + "\nVersion: " + version + "\nTitle: Test Package\nDepends: R (>= 3.5.0)\nImports: jsonlite,\n    httr\nLicense: MIT + file LICENSE\nNeedsCompilation: no\nURL: https://example.com, https://github.com/x/y\nDate/Publication: 2024-01-02 10:00:00 UTC\n" + extra
}

func srcPkg(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for fn, body := range map[string]string{
		name + "/DESCRIPTION": description(name, version, ""),
		name + "/R/x.R":       "f <- function() 1\n",
	} {
		_ = tw.WriteHeader(&tar.Header{Name: fn, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func winPkg(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(name + "/DESCRIPTION")
	_, _ = io.WriteString(w, description(name, version, "Built: R 4.4.0; ; 2024-01-02 10:00:00 UTC; windows\n"))
	_ = zw.Close()
	return buf.Bytes()
}

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "cran", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func packages(t *testing.T, h http.Handler, dir string) []*Stanza {
	t.Helper()
	w := do(h, http.MethodGet, "/"+dir+"/PACKAGES.gz", nil)
	if w.Code != 200 {
		t.Fatalf("PACKAGES.gz %s: %d %s", dir, w.Code, w.Body)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	st, err := ParseDCF(zr)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCRANLocal(t *testing.T) {
	l := newLocal(t, "r")
	ctx := context.Background()

	v1, v2 := srcPkg(t, "mypkg", "1.0-2"), srcPkg(t, "mypkg", "1.0-10")
	if w := do(l, http.MethodPut, "/upload", v1); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload", v2); w.Code != http.StatusCreated {
		t.Fatalf("upload v2: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload", srcPkg(t, "other", "0.1")); w.Code != http.StatusCreated {
		t.Fatalf("upload other: %d", w.Code)
	}
	if w := do(l, http.MethodPut, "/upload/bin/windows/4.4", winPkg(t, "mypkg", "1.0-10")); w.Code != http.StatusCreated {
		t.Fatalf("upload bin: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload", []byte("nope")); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage: %d", w.Code)
	}

	st := packages(t, l, "src/contrib")
	if len(st) != 2 || st[0].Get("Package") != "mypkg" || st[0].Get("Version") != "1.0-10" || st[0].Get("Imports") != "jsonlite, httr" || st[0].Get("MD5sum") == "" {
		t.Fatalf("PACKAGES: %+v", st[0])
	}
	w := do(l, http.MethodGet, "/src/contrib/PACKAGES", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "NeedsCompilation: no") {
		t.Fatalf("PACKAGES plain: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/src/contrib/PACKAGES.rds", nil); w.Code != 404 {
		t.Fatalf("rds: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/src/contrib/mypkg_1.0-10.tar.gz", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), v2) {
		t.Fatalf("latest download: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/src/contrib/Archive/mypkg/mypkg_1.0-2.tar.gz", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), v1) {
		t.Fatalf("archive download: %d", w.Code)
	}
	bst := packages(t, l, "bin/windows/contrib/4.4")
	if len(bst) != 1 || !strings.HasPrefix(bst[0].Get("Built"), "R 4.4.0") {
		t.Fatalf("bin PACKAGES: %+v", bst)
	}
	if w := do(l, http.MethodGet, "/bin/windows/contrib/4.4/mypkg_1.0-10.zip", nil); w.Code != 200 {
		t.Fatalf("bin download: %d", w.Code)
	}

	pkgs, _ := l.ListPackages(ctx)
	if len(pkgs) != 2 || pkgs[0].Name != "mypkg" || len(pkgs[0].Versions) != 2 || pkgs[0].Versions[1] != "1.0-10" {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(ctx, "mypkg")
	if err != nil || d.Generic.LatestVersion != "1.0-10" || d.Generic.Homepage != "https://example.com" || len(d.Generic.Versions[0].Files) != 2 {
		t.Fatalf("detail: %+v %v", d, err)
	}
	ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/src/contrib/Archive/mypkg/mypkg_1.0-2.tar.gz", nil))
	if !ok || ref.Name != "mypkg" || ref.Version != "1.0-2" {
		t.Fatalf("classify: %+v", ref)
	}
	if _, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/src/contrib/PACKAGES", nil)); ok {
		t.Fatal("classify index")
	}
	meta, err := l.ArtifactInfo(ctx, registry.ArtifactRef{Name: "mypkg", Version: "1.0-2"})
	if err != nil || meta.License != "MIT + file LICENSE" || meta.PublishedAt.Year() != 2024 {
		t.Fatalf("info: %+v %v", meta, err)
	}

	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(ctx, dst, "mypkg", "1.0-2"); err != nil {
		t.Fatal(err)
	}
	if st := packages(t, dst, "src/contrib"); len(st) != 1 || st[0].Get("Version") != "1.0-2" {
		t.Fatalf("promoted: %+v", st)
	}

	// Deleting the latest version moves the archived one back.
	if err := l.DeleteVersion(ctx, "mypkg", "1.0-10"); err != nil {
		t.Fatal(err)
	}
	if st := packages(t, l, "src/contrib"); st[0].Get("Version") != "1.0-2" {
		t.Fatalf("after delete: %+v", st[0])
	}
	if st := packages(t, l, "bin/windows/contrib/4.4"); len(st) != 0 {
		t.Fatalf("bin after delete: %+v", st)
	}
	if w := do(l, http.MethodGet, "/src/contrib/mypkg_1.0-2.tar.gz", nil); w.Code != 200 {
		t.Fatalf("restored latest: %d", w.Code)
	}
	if err := l.DeleteVersion(ctx, "mypkg", "1.0-10"); err != registry.ErrPackageNotFound {
		t.Fatalf("delete again: %v", err)
	}
	if w := do(l, http.MethodDelete, "/src/contrib/other_0.1.tar.gz", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete file: %d", w.Code)
	}
	if st := packages(t, l, "src/contrib"); len(st) != 1 {
		t.Fatalf("after file delete: %+v", st)
	}
}

func TestCRANRemoteAndVirtual(t *testing.T) {
	up := newLocal(t, "up")
	do(up, http.MethodPut, "/upload", srcPkg(t, "remo", "2.0"))
	srv := httptest.NewServer(up)
	defer srv.Close()

	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	if st := packages(t, rr, "src/contrib"); len(st) != 1 || st[0].Get("Package") != "remo" {
		t.Fatalf("remote PACKAGES: %+v", st)
	}
	// Upstream moves 2.0 to Archive; the client still asks for the contrib path.
	do(up, http.MethodPut, "/upload", srcPkg(t, "remo", "2.1"))
	if w := do(rr, http.MethodGet, "/src/contrib/remo_2.0.tar.gz", nil); w.Code != 200 {
		t.Fatalf("remote archive fallback: %d %s", w.Code, w.Body)
	}
	if err := rr.Prefetch(context.Background(), "remo", "2.1"); err != nil {
		t.Fatal(err)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || len(pkgs[0].Versions) != 2 {
		t.Fatalf("remote list: %+v", pkgs)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/src/contrib/remo_2.1.tar.gz", nil); w.Code != 200 {
		t.Fatalf("cached: %d", w.Code)
	}

	a, b := newLocal(t, "a"), newLocal(t, "b")
	do(a, http.MethodPut, "/upload", srcPkg(t, "shared", "1.0"))
	do(a, http.MethodPut, "/upload", srcPkg(t, "onlya", "1.0"))
	do(b, http.MethodPut, "/upload", srcPkg(t, "shared", "1.1"))
	members := map[string]registry.Registry{"a": a, "b": b}
	vreg, err := NewVirtualFactory(resolverFunc(func(_, n string) (registry.Registry, bool) {
		r, ok := members[n]
		return r, ok
	}))(context.Background(), registry.Deps{}, "default", &service.RegistryRepository{Name: "v", Members: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	st := packages(t, vreg, "src/contrib")
	if len(st) != 2 || st[1].Get("Package") != "shared" || st[1].Get("Version") != "1.1" {
		t.Fatalf("virtual PACKAGES: %+v %+v", st[0], st[1])
	}
	if w := do(vreg, http.MethodGet, "/src/contrib/PACKAGES", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "onlya") {
		t.Fatalf("virtual plain: %d %s", w.Code, w.Body)
	}
	if w := do(vreg, http.MethodGet, "/src/contrib/shared_1.1.tar.gz", nil); w.Code != 200 {
		t.Fatalf("virtual download: %d", w.Code)
	}
}

type resolverFunc func(ns, name string) (registry.Registry, bool)

func (f resolverFunc) Lookup(ns, name string) (registry.Registry, bool) { return f(ns, name) }
