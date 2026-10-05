package p2

import (
	"archive/zip"
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

func deps(t *testing.T) registry.Deps {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
}

func newLocal(t *testing.T) *Local {
	t.Helper()
	repo := &service.RegistryRepository{Name: "eclipse", Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "p2", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/eclipse")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func siteZip(t *testing.T, prefix string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := zw.Create(prefix + name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func basicSite(t *testing.T, prefix string) []byte {
	return siteZip(t, prefix, map[string]string{
		"content.xml":                      "<repository/>",
		"artifacts.xml":                    "<repository/>",
		"p2.index":                         "version=1\n",
		"plugins/com.example.a_1.0.0.jar":  "PLUGIN",
		"features/com.example.f_1.0.0.jar": "FEATURE",
	})
}

func TestLocalUploadAndComposite(t *testing.T) {
	l := newLocal(t)
	if w := do(l, http.MethodPut, "/upload/tools", basicSite(t, "")); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	// Wrapped in a single top-level directory.
	if w := do(l, http.MethodPut, "/upload/extras", basicSite(t, "repo/")); w.Code != http.StatusCreated {
		t.Fatalf("upload wrapped: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload/bad", siteZip(t, "", map[string]string{"x.txt": "x"})); w.Code != http.StatusBadRequest {
		t.Fatalf("non-p2 zip: %d", w.Code)
	}
	if w := do(l, http.MethodPut, "/upload/evil", siteZip(t, "", map[string]string{"../content.xml": "x"})); w.Code != http.StatusBadRequest {
		t.Fatalf("traversal zip: %d", w.Code)
	}

	w := do(l, http.MethodGet, "/compositeContent.xml", nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "<children size='2'>") || !strings.Contains(body, "location='tools/'") || !strings.Contains(body, "location='extras/'") || !strings.Contains(body, metaRepoType) {
		t.Fatalf("compositeContent: %d %s", w.Code, body)
	}
	if w := do(l, http.MethodGet, "/compositeArtifacts.xml", nil); !strings.Contains(w.Body.String(), artifactRepoType) {
		t.Fatalf("compositeArtifacts: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/p2.index", nil); !strings.Contains(w.Body.String(), "compositeContent.xml") {
		t.Fatalf("p2.index: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/extras/plugins/com.example.a_1.0.0.jar", nil); w.Code != 200 || w.Body.String() != "PLUGIN" {
		t.Fatalf("plugin: %d %q", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/tools/plugins/com.example.b_2.0.0.jar", []byte("B")); w.Code != http.StatusCreated {
		t.Fatalf("single put: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/tools/plugins/com.example.b_2.0.0.jar", nil); w.Body.String() != "B" {
		t.Fatalf("single get: %q", w.Body)
	}

	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 2 || pkgs[0].Name != "extras" || len(pkgs[0].Versions) != 1 {
		t.Fatalf("list: %+v", pkgs)
	}
	ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/tools/plugins/com.example.a_1.0.0.jar", nil))
	if !ok || ref.Name != "tools" || ref.Version == "" {
		t.Fatalf("classify: %+v %v", ref, ok)
	}
	if _, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/compositeContent.xml", nil)); ok {
		t.Fatal("root doc should not classify")
	}
	d, err := l.PackageDetail(context.Background(), "tools")
	if err != nil || d.Generic.Versions[0].Metadata["plugins"] != "2" {
		t.Fatalf("detail: %+v %v", d, err)
	}

	dst := newLocal(t)
	if err := l.PromoteVersion(context.Background(), dst, "tools", ""); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/tools/features/com.example.f_1.0.0.jar", nil); w.Body.String() != "FEATURE" {
		t.Fatalf("promoted: %d %q", w.Code, w.Body)
	}

	if err := l.DeleteVersion(context.Background(), "tools", "nope"); err != registry.ErrPackageNotFound {
		t.Fatalf("wrong version delete: %v", err)
	}
	if err := l.DeleteVersion(context.Background(), "tools", pkgs[1].Versions[0]); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/compositeContent.xml", nil); strings.Contains(w.Body.String(), "tools/") || !strings.Contains(w.Body.String(), "<children size='1'>") {
		t.Fatalf("composite after delete: %s", w.Body)
	}
	if w := do(l, http.MethodDelete, "/extras", nil); w.Code != http.StatusNoContent {
		t.Fatalf("http delete: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/extras/content.xml", nil); w.Code != 404 {
		t.Fatalf("after delete: %d", w.Code)
	}
}

func TestRemote(t *testing.T) {
	up := newLocal(t)
	do(up, http.MethodPut, "/upload/site", basicSite(t, ""))
	srv := httptest.NewServer(up)
	defer srv.Close()

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	if w := do(rr, http.MethodGet, "/compositeContent.xml", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "location='site/'") {
		t.Fatalf("composite: %d %s", w.Code, w.Body)
	}
	if err := rr.Prefetch(context.Background(), "site", ""); err != nil {
		t.Fatal(err)
	}
	if w := do(rr, http.MethodGet, "/site/plugins/com.example.a_1.0.0.jar", nil); w.Code != 200 || w.Body.String() != "PLUGIN" {
		t.Fatalf("plugin: %d %q", w.Code, w.Body)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/site/plugins/com.example.a_1.0.0.jar", nil); w.Code != 200 {
		t.Fatalf("cached plugin: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/site/content.xml", nil); w.Code != 200 {
		t.Fatalf("cached metadata: %d", w.Code)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "com.example.a" || pkgs[0].Versions[0] != "1.0.0" {
		t.Fatalf("list: %+v", pkgs)
	}
	if ref, ok := rr.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/site/plugins/com.example.a_1.0.0.jar", nil)); !ok || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}
	if w := do(rr, http.MethodPut, "/upload/x", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote put: %d", w.Code)
	}
}

type stubResolver map[string]registry.Registry

func (s stubResolver) Lookup(_, repo string) (registry.Registry, bool) {
	r, ok := s[repo]
	return r, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t), newLocal(t)
	do(a, http.MethodPut, "/upload/one", basicSite(t, ""))
	do(b, http.MethodPut, "/upload/two", basicSite(t, ""))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(stubResolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	w := do(reg, http.MethodGet, "/compositeContent.xml", nil)
	if !strings.Contains(w.Body.String(), "location='a/'") || !strings.Contains(w.Body.String(), "location='b/'") {
		t.Fatalf("virtual composite: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/a/compositeContent.xml", nil); !strings.Contains(w.Body.String(), "location='one/'") {
		t.Fatalf("member composite: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/b/two/plugins/com.example.a_1.0.0.jar", nil); w.Code != 200 || w.Body.String() != "PLUGIN" {
		t.Fatalf("member file: %d %q", w.Code, w.Body)
	}
	if w := do(reg, http.MethodGet, "/c/x", nil); w.Code != 404 {
		t.Fatalf("unknown member: %d", w.Code)
	}
	if w := do(reg, http.MethodPut, "/a/upload/x", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual put: %d", w.Code)
	}
	pkgs, _ := reg.(registry.PackageLister).ListPackages(context.Background())
	if len(pkgs) != 2 {
		t.Fatalf("lister: %+v", pkgs)
	}
}
