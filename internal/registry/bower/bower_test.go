package bower

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	repo := &service.RegistryRepository{Name: "bw", Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "bower", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func req(h http.Handler, method, p, ct string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/bw")
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func register(t *testing.T, h http.Handler, name, u string) {
	t.Helper()
	form := url.Values{"name": {name}, "url": {u}}
	if w := req(h, http.MethodPost, "/packages", "application/x-www-form-urlencoded", []byte(form.Encode())); w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
}

func tgz(t *testing.T, bowerJSON string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "pkg/bower.json", Mode: 0o644, Size: int64(len(bowerJSON)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(bowerJSON))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestLocalRegistry(t *testing.T) {
	l := newLocal(t)
	register(t, l, "jquery", "https://github.com/jquery/jquery-dist.git")
	register(t, l, "lodash", "https://github.com/lodash/lodash.git")

	form := url.Values{"name": {"jquery"}, "url": {"https://evil.example/x.git"}}
	if w := req(l, http.MethodPost, "/packages", "application/x-www-form-urlencoded", []byte(form.Encode())); w.Code != http.StatusForbidden {
		t.Fatalf("re-register: %d", w.Code)
	}
	w := req(l, http.MethodGet, "/packages/jquery", "", nil)
	var e Entry
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &e) != nil || e.URL != "https://github.com/jquery/jquery-dist.git" {
		t.Fatalf("entry: %d %s", w.Code, w.Body)
	}
	w = req(l, http.MethodGet, "/packages/search/lod", "", nil)
	if !strings.Contains(w.Body.String(), `"lodash"`) || strings.Contains(w.Body.String(), "jquery") {
		t.Fatalf("search: %s", w.Body)
	}
	if w := req(l, http.MethodDelete, "/packages/lodash", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := req(l, http.MethodGet, "/packages/lodash", "", nil); w.Code != 404 {
		t.Fatalf("after delete: %d", w.Code)
	}
}

func TestLocalArchives(t *testing.T) {
	l := newLocal(t)
	a1 := tgz(t, `{"name":"widget","license":"MIT","description":"d"}`)
	a2 := tgz(t, `{"name":"widget","license":"Apache-2.0"}`)
	if w := req(l, http.MethodPut, "/archives/widget/1.0.0", "", a1); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := req(l, http.MethodPut, "/archives/widget/1.2.0", "", a2); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	w := req(l, http.MethodGet, "/packages/widget", "", nil)
	if !strings.Contains(w.Body.String(), `"url":"http://example.com/registries/default/bw/archives/widget/latest.tar.gz"`) {
		t.Fatalf("entry: %s", w.Body)
	}
	if w := req(l, http.MethodGet, "/archives/widget/latest.tar.gz", "", nil); !bytes.Equal(w.Body.Bytes(), a2) {
		t.Fatalf("latest: %d", w.Code)
	}
	if w := req(l, http.MethodGet, "/archives/widget/1.0.0.tar.gz", "", nil); !bytes.Equal(w.Body.Bytes(), a1) {
		t.Fatalf("pinned: %d", w.Code)
	}
	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 1 || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	info, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "widget", Version: "1.0.0"})
	if err != nil || info.License != "MIT" {
		t.Fatalf("info: %+v %v", info, err)
	}
	d, err := l.PackageDetail(context.Background(), "widget")
	if err != nil || d.Generic.LatestVersion != "1.2.0" {
		t.Fatalf("detail: %+v %v", d, err)
	}

	dst := newLocal(t)
	if err := l.PromoteVersion(context.Background(), dst, "widget", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := req(dst, http.MethodGet, "/archives/widget/latest.tar.gz", "", nil); !bytes.Equal(w.Body.Bytes(), a1) {
		t.Fatalf("promoted: %d", w.Code)
	}

	if err := l.DeleteVersion(context.Background(), "widget", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if w := req(l, http.MethodGet, "/archives/widget/latest.tar.gz", "", nil); !bytes.Equal(w.Body.Bytes(), a1) {
		t.Fatalf("latest after delete: %d", w.Code)
	}
	if err := l.DeleteVersion(context.Background(), "widget", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := req(l, http.MethodGet, "/packages/widget", "", nil); w.Code != 404 {
		t.Fatalf("hosted entry should be gone: %d", w.Code)
	}
}

func TestRemote(t *testing.T) {
	up := newLocal(t)
	register(t, up, "jquery", "https://github.com/jquery/jquery-dist.git")
	req(up, http.MethodPut, "/archives/widget/1.0.0", "", tgz(t, `{}`))
	srv := httptest.NewServer(up)
	defer srv.Close()

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	if w := req(rr, http.MethodGet, "/packages/jquery", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "jquery-dist") {
		t.Fatalf("entry: %d %s", w.Code, w.Body)
	}
	w := req(rr, http.MethodGet, "/packages/widget", "", nil)
	if strings.Contains(w.Body.String(), srv.URL) || !strings.Contains(w.Body.String(), "/registries/default/bw/archives/widget/") {
		t.Fatalf("archive url not rewritten: %s", w.Body)
	}
	if err := rr.Prefetch(context.Background(), "widget", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if w := req(rr, http.MethodGet, "/archives/widget/1.0.0.tar.gz", "", nil); w.Code != 200 {
		t.Fatalf("cached archive: %d", w.Code)
	}
	if w := req(rr, http.MethodGet, "/packages/jquery", "", nil); w.Code != 200 {
		t.Fatalf("stale entry: %d", w.Code)
	}
}

type stubResolver map[string]registry.Registry

func (s stubResolver) Lookup(_, repo string) (registry.Registry, bool) {
	r, ok := s[repo]
	return r, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t), newLocal(t)
	register(t, a, "jquery", "https://a.example/jquery.git")
	register(t, b, "jquery", "https://b.example/jquery.git")
	register(t, b, "lodash", "https://b.example/lodash.git")
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(stubResolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	if w := req(reg, http.MethodGet, "/packages/jquery", "", nil); !strings.Contains(w.Body.String(), "a.example") {
		t.Fatalf("first hit: %s", w.Body)
	}
	var entries []Entry
	w := req(reg, http.MethodGet, "/packages", "", nil)
	if json.Unmarshal(w.Body.Bytes(), &entries) != nil || len(entries) != 2 || entries[0].URL != "https://a.example/jquery.git" {
		t.Fatalf("list: %s", w.Body)
	}
	pkgs, _ := reg.(registry.PackageLister).ListPackages(context.Background())
	if len(pkgs) != 2 {
		t.Fatalf("lister: %+v", pkgs)
	}
}
