package conan

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "conan", AllowPush: true}
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

const recipe = "/v2/conans/zlib/1.3/_/_"

func uploadRecipe(t *testing.T, h http.Handler, ref, rrev, conanfile string) {
	t.Helper()
	for f, body := range map[string]string{"conanfile.py": conanfile, "conanmanifest.txt": "manifest " + rrev} {
		if w := do(h, http.MethodPut, ref+"/revisions/"+rrev+"/files/"+f, []byte(body)); w.Code != http.StatusCreated {
			t.Fatalf("put %s: %d %s", f, w.Code, w.Body)
		}
	}
}

func TestConanLocalRoundTrip(t *testing.T) {
	l := newLocal(t, "c")
	ctx := context.Background()

	w := do(l, http.MethodGet, "/v1/ping", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("X-Conan-Server-Capabilities"), "revisions") {
		t.Fatalf("ping: %d %v", w.Code, w.Header())
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/users/authenticate", nil)
	req.SetBasicAuth("x", "kutu_tok")
	w = httptest.NewRecorder()
	l.ServeHTTP(w, req)
	if w.Body.String() != "kutu_tok" {
		t.Fatalf("auth: %q", w.Body)
	}

	uploadRecipe(t, l, recipe, "rev1", `class Z: license = "Zlib"`+"\n"+`    description = "compression"`)
	uploadRecipe(t, l, recipe, "rev2", `license = "Zlib"`)
	uploadRecipe(t, l, "/v2/conans/zlib/1.2/acme/stable", "r0", `license = "MIT"`)

	pkg := recipe + "/revisions/rev2/packages/abc123/revisions/p1/files/"
	for f, b := range map[string]string{"conaninfo.txt": "[settings]\nos=Linux\n", "conan_package.tgz": "tgz", "conanmanifest.txt": "m"} {
		if w := do(l, http.MethodPut, pkg+f, []byte(b)); w.Code != http.StatusCreated {
			t.Fatalf("put pkg %s: %d %s", f, w.Code, w.Body)
		}
	}

	w = do(l, http.MethodGet, recipe+"/revisions", nil)
	var revs revisionList
	_ = json.Unmarshal(w.Body.Bytes(), &revs)
	if w.Code != 200 || len(revs.Revisions) != 2 || revs.Revisions[0].Revision != "rev2" || revs.Reference != "zlib/1.3" {
		t.Fatalf("revisions: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, recipe+"/latest", nil); !strings.Contains(w.Body.String(), `"rev2"`) {
		t.Fatalf("latest: %s", w.Body)
	}
	if w := do(l, http.MethodGet, recipe+"/revisions/rev1/files", nil); !strings.Contains(w.Body.String(), `"conanfile.py":{}`) {
		t.Fatalf("files: %s", w.Body)
	}
	if w := do(l, http.MethodGet, recipe+"/revisions/rev2/files/conanfile.py", nil); w.Body.String() != `license = "Zlib"` {
		t.Fatalf("file: %q", w.Body)
	}
	if w := do(l, http.MethodGet, recipe+"/revisions/rev2/packages/abc123/latest", nil); !strings.Contains(w.Body.String(), `"p1"`) {
		t.Fatalf("pkg latest: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, pkg+"conan_package.tgz", nil); w.Body.String() != "tgz" {
		t.Fatalf("pkg file: %q", w.Body)
	}
	if w := do(l, http.MethodGet, recipe+"/revisions/rev2/search", nil); !strings.Contains(w.Body.String(), `"abc123":{"settings":{"os":"Linux"}}`) {
		t.Fatalf("pkg search: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/v2/conans/search?q=zlib*", nil); !strings.Contains(w.Body.String(), `"zlib/1.2@acme/stable","zlib/1.3"`) {
		t.Fatalf("search: %s", w.Body)
	}

	pkgs, _ := l.ListPackages(ctx)
	if len(pkgs) != 1 || pkgs[0].Name != "zlib" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(ctx, "zlib")
	if err != nil || d.Generic.LatestVersion != "1.3" || d.Generic.License != "Zlib" || d.Generic.Versions[0].Metadata["latest_revision"] != "rev2" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, pkg+"conan_package.tgz", nil)); !ok || ref.Name != "zlib" || ref.Version != "1.3" {
		t.Fatalf("classify: %+v", ref)
	}

	if w := do(l, http.MethodDelete, recipe+"/revisions/rev2", nil); w.Code != 200 {
		t.Fatalf("delete rev: %d", w.Code)
	}
	if w := do(l, http.MethodGet, recipe+"/latest", nil); !strings.Contains(w.Body.String(), `"rev1"`) {
		t.Fatalf("latest after delete: %s", w.Body)
	}

	if err := l.DeleteVersion(ctx, "zlib", "1.2@acme/stable"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/v2/conans/zlib/1.2/acme/stable/revisions", nil); w.Code != 404 {
		t.Fatalf("after delete version: %d", w.Code)
	}
	if err := l.DeleteVersion(ctx, "zlib", "9.9"); err != registry.ErrPackageNotFound {
		t.Fatalf("missing delete: %v", err)
	}

	dst := newLocal(t, "d")
	if err := l.PromoteVersion(ctx, dst, "zlib", "1.3"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, recipe+"/latest", nil); !strings.Contains(w.Body.String(), `"rev1"`) {
		t.Fatalf("promoted: %s", w.Body)
	}
}

func TestConanRemote(t *testing.T) {
	up := newLocal(t, "up")
	uploadRecipe(t, up, recipe, "rev1", `license = "Zlib"`)
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
	if w := do(rr, http.MethodGet, recipe+"/latest", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "rev1") {
		t.Fatalf("remote latest: %d %s", w.Code, w.Body)
	}
	if w := do(rr, http.MethodGet, recipe+"/revisions/rev1/files/conanfile.py", nil); w.Code != 200 || w.Body.String() != `license = "Zlib"` {
		t.Fatalf("remote file: %d %q", w.Code, w.Body)
	}
	if w := do(rr, http.MethodPut, recipe+"/revisions/rev1/files/x", []byte("x")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote put: %d", w.Code)
	}
	if err := rr.Prefetch(context.Background(), "zlib", ""); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, recipe+"/revisions/rev1/files/conanmanifest.txt", nil); w.Code != 200 || w.Body.String() != "manifest rev1" {
		t.Fatalf("cached file: %d %q", w.Code, w.Body)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "zlib" {
		t.Fatalf("remote list: %+v", pkgs)
	}
	st, _ := rr.PurgeCache(context.Background(), registry.PurgeOptions{})
	if st.PurgedFiles == 0 || !rr.store.Exists("conans/zlib/1.3/_/_/rev1/files/conanfile.py") {
		t.Fatalf("purge: %+v", st)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestConanVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	uploadRecipe(t, a, recipe, "ra", "a")
	uploadRecipe(t, b, recipe, "rb", "b")
	uploadRecipe(t, b, "/v2/conans/fmt/10.0/_/_", "rf", "f")

	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	w := do(reg, http.MethodGet, recipe+"/revisions", nil)
	var revs revisionList
	_ = json.Unmarshal(w.Body.Bytes(), &revs)
	if len(revs.Revisions) != 2 || revs.Revisions[0].Revision != "rb" {
		t.Fatalf("virtual revisions: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, recipe+"/latest", nil); !strings.Contains(w.Body.String(), `"rb"`) {
		t.Fatalf("virtual latest: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/v2/conans/search?q=*", nil); !strings.Contains(w.Body.String(), `["fmt/10.0","zlib/1.3"]`) {
		t.Fatalf("virtual search: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, recipe+"/revisions/ra/files/conanfile.py", nil); w.Body.String() != "a" {
		t.Fatalf("virtual file: %q", w.Body)
	}
}
