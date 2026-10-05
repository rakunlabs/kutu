package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
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

const prefix = "/registries/default/php"

func deps(t *testing.T) registry.Deps {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
}

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "php", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func req(method, p string, body []byte) *http.Request {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("X-Pika-Registry-Prefix", prefix)
	return r
}

func do(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(method, p, body))
	return w
}

func pkgZip(t *testing.T, dir, composerJSON string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create(dir + "composer.json")
	_, _ = f.Write([]byte(composerJSON))
	g, _ := zw.Create(dir + "src/Foo.php")
	_, _ = g.Write([]byte("<?php class Foo {}"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const cj = `{"name":"Acme/Foo","description":"Foo lib","license":"MIT","type":"library","require":{"php":">=8.1"},"autoload":{"psr-4":{"Acme\\Foo\\":"src/"}},"keywords":["fooish"]}`

type p2 struct {
	Packages map[string][]map[string]any `json:"packages"`
}

func decodeP2(t *testing.T, body []byte, name string) []map[string]any {
	t.Helper()
	var d p2
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return d.Packages[name]
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"1.0.0":      "1.0.0.0",
		"v2.1":       "2.1.0.0",
		"1.2.3-RC1":  "1.2.3.0-RC1",
		"1.0.0-beta": "1.0.0.0-beta",
		"dev-main":   "dev-main",
		"2.x-dev":    "2.9999999.9999999.9999999-dev",
	} {
		if got := NormalizeVersion(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestLocalPublishMetadataDownloadDelete(t *testing.T) {
	l := newLocal(t, "php")
	z := pkgZip(t, "foo-1.0.0/", cj)
	if w := do(l, http.MethodPut, "/api/upload?version=1.0.0", z); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	// version from composer.json, multipart field "file".
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "foo.zip")
	_, _ = fw.Write(pkgZip(t, "", strings.Replace(cj, `"type"`, `"version":"1.10.0","type"`, 1)))
	_ = mw.Close()
	r := req(http.MethodPost, "/api/upload", buf.Bytes())
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	l.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("multipart upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/api/upload?version=dev-main", z); w.Code != http.StatusCreated {
		t.Fatalf("dev upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/api/upload", pkgZip(t, "", `{"name":"acme/bar"}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("missing version: %d", w.Code)
	}

	w = do(l, http.MethodGet, "/packages.json", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"metadata-url":"`+prefix+`/p2/%package%.json"`) || !strings.Contains(w.Body.String(), `"available-packages":["acme/foo"]`) {
		t.Fatalf("root: %d %s", w.Code, w.Body)
	}

	w = do(l, http.MethodGet, "/p2/acme/foo.json", nil)
	vs := decodeP2(t, w.Body.Bytes(), "acme/foo")
	if w.Code != 200 || len(vs) != 2 || vs[0]["version"] != "1.10.0" || vs[1]["version_normalized"] != "1.0.0.0" {
		t.Fatalf("p2: %d %s", w.Code, w.Body)
	}
	dist := vs[1]["dist"].(map[string]any)
	if dist["url"] != "http://example.com"+prefix+"/dist/acme/foo/1.0.0.zip" || dist["shasum"] != sha1Hex(z) || dist["type"] != "zip" {
		t.Fatalf("dist: %+v", dist)
	}
	if vs[1]["autoload"] == nil || vs[1]["license"] != "MIT" {
		t.Fatalf("fields: %+v", vs[1])
	}
	dev := decodeP2(t, do(l, http.MethodGet, "/p2/acme/foo~dev.json", nil).Body.Bytes(), "acme/foo")
	if len(dev) != 1 || dev[0]["version"] != "dev-main" {
		t.Fatalf("dev: %+v", dev)
	}

	if w := do(l, http.MethodGet, "/dist/acme/foo/1.0.0.zip", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), z) {
		t.Fatalf("dist get: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/search.json?q=fooish", nil); !strings.Contains(w.Body.String(), `"name":"acme/foo"`) || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("search: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/search.json?q=nomatch", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("search miss: %s", w.Body)
	}

	ref, ok := l.ClassifyRequest(req(http.MethodGet, "/dist/acme/foo/1.0.0.zip", nil))
	if !ok || ref.Name != "acme/foo" || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}
	meta, err := l.ArtifactInfo(context.Background(), ref)
	if err != nil || meta.License != "MIT" || meta.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", meta, err)
	}
	d, err := l.PackageDetail(context.Background(), "acme/foo")
	if err != nil || d.Generic.LatestVersion != "1.10.0" || len(d.Generic.Versions) != 3 {
		t.Fatalf("detail: %+v %v", d, err)
	}

	if w := do(l, http.MethodDelete, "/api/packages/acme/foo/1.0.0", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	vs = decodeP2(t, do(l, http.MethodGet, "/p2/acme/foo.json", nil).Body.Bytes(), "acme/foo")
	if len(vs) != 1 || vs[0]["version"] != "1.10.0" {
		t.Fatalf("after delete: %+v", vs)
	}
	if w := do(l, http.MethodGet, "/dist/acme/foo/1.0.0.zip", nil); w.Code != 404 {
		t.Fatalf("dist after delete: %d", w.Code)
	}
	for _, v := range []string{"1.10.0", "dev-main"} {
		if err := l.DeleteVersion(context.Background(), "acme/foo", v); err != nil {
			t.Fatal(err)
		}
	}
	if w := do(l, http.MethodGet, "/p2/acme/foo.json", nil); w.Code != 404 {
		t.Fatalf("p2 after full delete: %d", w.Code)
	}
	if err := l.DeleteVersion(context.Background(), "acme/foo", "9.9.9"); err != registry.ErrPackageNotFound {
		t.Fatalf("missing delete: %v", err)
	}
}

func TestPromote(t *testing.T) {
	src, dst := newLocal(t, "a"), newLocal(t, "b")
	do(src, http.MethodPut, "/api/upload?version=1.0.0", pkgZip(t, "", cj))
	if err := src.PromoteVersion(context.Background(), dst, "acme/foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/dist/acme/foo/1.0.0.zip", nil); w.Code != 200 {
		t.Fatalf("promoted dist: %d", w.Code)
	}
	if vs := decodeP2(t, do(dst, http.MethodGet, "/p2/acme/foo.json", nil).Body.Bytes(), "acme/foo"); len(vs) != 1 {
		t.Fatalf("promoted p2: %+v", vs)
	}
}

func TestRemote(t *testing.T) {
	zipBody := pkgZip(t, "acme-foo-abc/", cj)
	var distHits atomic.Int32
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("/packages.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"packages":[],"metadata-url":"/p2/%package%.json","search":"/search.json?q=%query%&type=%type%"}`))
	})
	// Minified upstream metadata with an absolute dist URL.
	mux.HandleFunc("/p2/acme/foo.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"minified":"composer/2.0","packages":{"acme/foo":[
			{"name":"acme/foo","version":"2.0.0","version_normalized":"2.0.0.0","license":["MIT"],"time":"2024-01-02T03:04:05+00:00","dist":{"type":"zip","url":"` + srvURL + `/zipball/abc","reference":"abc","shasum":""}},
			{"version":"1.0.0","version_normalized":"1.0.0.0","dist":{"type":"zip","url":"` + srvURL + `/zipball/old","reference":"old","shasum":""}}]}}`))
	})
	mux.HandleFunc("/p2/acme/foo~dev.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"packages":{"acme/foo":[]}}`))
	})
	mux.HandleFunc("/zipball/abc", func(w http.ResponseWriter, _ *http.Request) {
		distHits.Add(1)
		_, _ = w.Write(zipBody)
	})
	mux.HandleFunc("/search.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"name":"acme/foo","description":"d","url":"https://packagist.org/packages/acme/foo"}],"total":1}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)

	w := do(rr, http.MethodGet, "/p2/acme/foo.json", nil)
	vs := decodeP2(t, w.Body.Bytes(), "acme/foo")
	if w.Code != 200 || len(vs) != 2 || strings.Contains(w.Body.String(), srv.URL) {
		t.Fatalf("p2: %d %s", w.Code, w.Body)
	}
	if vs[1]["license"] == nil || vs[1]["name"] != "acme/foo" {
		t.Fatalf("minified expand: %+v", vs[1])
	}
	if u := vs[0]["dist"].(map[string]any)["url"]; u != "http://example.com"+prefix+"/dist/acme/foo/2.0.0.zip" {
		t.Fatalf("rewritten url: %v", u)
	}
	for i := 0; i < 2; i++ {
		if w := do(rr, http.MethodGet, "/dist/acme/foo/2.0.0.zip", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), zipBody) {
			t.Fatalf("dist: %d %s", w.Code, w.Body)
		}
	}
	if distHits.Load() != 1 {
		t.Fatalf("dist fetched %d times", distHits.Load())
	}
	if w := do(rr, http.MethodGet, "/search.json?q=foo", nil); !strings.Contains(w.Body.String(), prefix+"/p2/acme/foo.json") {
		t.Fatalf("search: %s", w.Body)
	}
	meta, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "acme/foo", Version: "2.0.0"})
	if err != nil || meta.License != "MIT" || meta.PublishedAt.Year() != 2024 {
		t.Fatalf("info: %+v %v", meta, err)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	if d, err := rr.PackageDetail(context.Background(), "acme/foo"); err != nil || d.Generic.LatestVersion != "2.0.0" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if w := do(rr, http.MethodPut, "/api/upload", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote write: %d", w.Code)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/dist/acme/foo/2.0.0.zip", nil); w.Code != 200 {
		t.Fatalf("cached dist offline: %d", w.Code)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	do(a, http.MethodPut, "/api/upload?version=1.0.0", pkgZip(t, "", cj))
	do(b, http.MethodPut, "/api/upload?version=1.0.0", pkgZip(t, "", strings.Replace(cj, "Foo lib", "other", 1)))
	do(b, http.MethodPut, "/api/upload?version=2.0.0", pkgZip(t, "", cj))
	do(b, http.MethodPut, "/api/upload?version=0.1.0", pkgZip(t, "", `{"name":"acme/bar"}`))

	repo := &service.RegistryRepository{Name: "virt", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	w := do(reg, http.MethodGet, "/p2/acme/foo.json", nil)
	vs := decodeP2(t, w.Body.Bytes(), "acme/foo")
	if len(vs) != 2 || vs[0]["version"] != "2.0.0" || vs[1]["description"] != "Foo lib" {
		t.Fatalf("merged: %s", w.Body)
	}
	if u := vs[0]["dist"].(map[string]any)["url"].(string); !strings.HasPrefix(u, "http://example.com"+prefix+"/dist/") {
		t.Fatalf("virtual dist url: %s", u)
	}
	if w := do(reg, http.MethodGet, "/dist/acme/foo/2.0.0.zip", nil); w.Code != 200 {
		t.Fatalf("virtual dist: %d", w.Code)
	}
	if w := do(reg, http.MethodGet, "/packages.json", nil); !strings.Contains(w.Body.String(), `"available-packages":["acme/bar","acme/foo"]`) {
		t.Fatalf("virtual root: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/search.json?q=acme", nil); !strings.Contains(w.Body.String(), `"total":2`) {
		t.Fatalf("virtual search: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/p2/acme/nope.json", nil); w.Code != 404 {
		t.Fatalf("virtual miss: %d", w.Code)
	}
}
