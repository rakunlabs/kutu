package generic

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

func newLocal(t *testing.T, policy *service.RegistryPolicy) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "gen", Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "generic", AllowPush: true, Policy: policy}
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

func TestGenericLocalRoundTrip(t *testing.T) {
	l := newLocal(t, nil)
	if w := do(l, http.MethodPut, "/tools/cli/1.2.0/cli-linux-amd64", []byte("bin")); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/tools/cli/1.10.0/cli-linux-amd64", []byte("bin2")); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/tools/cli/1.2.0/cli-linux-amd64", nil); w.Code != 200 || w.Body.String() != "bin" {
		t.Fatalf("get: %d %q", w.Code, w.Body)
	}
	w := do(l, http.MethodGet, "/tools/cli/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"latest":"1.10.0"`) {
		t.Fatalf("versions: %d %s", w.Code, w.Body)
	}
	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "tools/cli" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(context.Background(), "tools/cli")
	if err != nil || d.Generic.LatestVersion != "1.10.0" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if err := l.DeleteVersion(context.Background(), "tools/cli", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/tools/cli/1.2.0/cli-linux-amd64", nil); w.Code != 404 {
		t.Fatalf("after delete: %d", w.Code)
	}
}

func TestGenericImmutableAndQuota(t *testing.T) {
	l := newLocal(t, &service.RegistryPolicy{ImmutableVersions: true, QuotaBytes: 5})
	if w := do(l, http.MethodPut, "/a/1/f", []byte("abc")); w.Code != http.StatusCreated {
		t.Fatalf("put: %d", w.Code)
	}
	if w := do(l, http.MethodPut, "/a/1/f", []byte("abc")); w.Code != http.StatusConflict {
		t.Fatalf("immutable: %d", w.Code)
	}
	if w := do(l, http.MethodPut, "/a/2/f", []byte("abcdef")); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("quota: %d", w.Code)
	}
}

func TestGenericRemote(t *testing.T) {
	up := newLocal(t, nil)
	do(up, http.MethodPut, "/x/1.0/file.txt", []byte("hello"))
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
	if w := do(rr, http.MethodGet, "/x/1.0/file.txt", nil); w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("remote get: %d %q", w.Code, w.Body)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/x/1.0/file.txt", nil); w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("cached get: %d %q", w.Code, w.Body)
	}
	if err := rr.Prefetch(context.Background(), "x", "1.0"); err != nil {
		// upstream is down; listing not cached yet → error is expected
		_ = err
	}
}

func TestGenericPromote(t *testing.T) {
	src := newLocal(t, nil)
	dst := newLocal(t, nil)
	do(src, http.MethodPut, "/a/1.0/f", []byte("x"))
	if err := src.PromoteVersion(context.Background(), dst, "a", "1.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/a/1.0/f", nil); w.Body.String() != "x" {
		t.Fatalf("promoted: %d %q", w.Code, w.Body)
	}
	if err := src.PromoteVersion(context.Background(), dst, "a", "9"); err != registry.ErrPackageNotFound {
		t.Fatalf("want not found, got %v", err)
	}
}
