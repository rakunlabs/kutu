package puppet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

func moduleTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{
		"name": name, "version": version, "author": "acme", "license": "Apache-2.0", "summary": "test module",
		"dependencies": []map[string]string{{"name": "puppetlabs/stdlib", "version_requirement": ">= 4.0.0"}},
	})
	dir := strings.Replace(name, "/", "-", 1) + "-" + version
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, body := range map[string]string{dir + "/metadata.json": string(meta), dir + "/manifests/init.pp": "class foo {}\n"} {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func newFS(t *testing.T) registry.Deps {
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
}

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: name, AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), newFS(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, body)
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/forge")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func publish(t *testing.T, h http.Handler, tarball []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "m.tar.gz")
	_, _ = fw.Write(tarball)
	_ = mw.Close()
	return do(h, http.MethodPost, "/v3/releases", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return m
}

func TestPuppetLocal(t *testing.T) {
	l := newLocal(t, "forge")
	if w := publish(t, l, moduleTarball(t, "acme-foo", "1.0.0")); w.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if w := publish(t, l, moduleTarball(t, "acme/foo", "1.2.0")); w.Code != http.StatusCreated {
		t.Fatalf("publish2: %d %s", w.Code, w.Body)
	}
	w := do(l, http.MethodGet, "/v3/modules/acme-foo", nil, nil)
	if w.Code != 200 {
		t.Fatalf("module: %d %s", w.Code, w.Body)
	}
	mod := decode(t, w)
	if mod["current_release"].(map[string]any)["version"] != "1.2.0" || len(mod["releases"].([]any)) != 2 {
		t.Fatalf("module doc: %s", w.Body)
	}
	w = do(l, http.MethodGet, "/v3/releases?module=acme-foo&limit=1&sort_by=version", nil, nil)
	pg := decode(t, w)
	pag := pg["pagination"].(map[string]any)
	if pag["total"].(float64) != 2 || pag["next"] == nil {
		t.Fatalf("releases: %s", w.Body)
	}
	next := pag["next"].(string)
	if !strings.HasPrefix(next, "/v3/releases?") {
		t.Fatalf("next: %s", next)
	}
	w = do(l, http.MethodGet, next, nil, nil)
	if pg2 := decode(t, w); pg2["pagination"].(map[string]any)["next"] != nil || len(pg2["results"].([]any)) != 1 {
		t.Fatalf("page2: %s", w.Body)
	}
	w = do(l, http.MethodGet, "/v3/releases/acme-foo-1.0.0", nil, nil)
	rel := decode(t, w)
	if rel["file_uri"] != "/v3/files/acme-foo-1.0.0.tar.gz" || rel["file_sha256"] == "" || rel["file_md5"] == "" {
		t.Fatalf("release: %s", w.Body)
	}
	if deps := rel["metadata"].(map[string]any)["dependencies"].([]any); len(deps) != 1 {
		t.Fatalf("deps: %v", deps)
	}
	w = do(l, http.MethodGet, "/v3/files/acme-foo-1.0.0.tar.gz", nil, nil)
	if w.Code != 200 || pkgbase.SHA256Hex(w.Body.Bytes()) != rel["file_sha256"] {
		t.Fatalf("file: %d", w.Code)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/v3/files/acme-foo-1.0.0.tar.gz", nil)); !ok || ref.Name != "acme-foo" || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}
	if w := do(l, http.MethodGet, "/v3/modules?query=foo", nil, nil); w.Code != 200 || len(decode(t, w)["results"].([]any)) != 1 {
		t.Fatalf("search: %s", w.Body)
	}
	if info, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "acme-foo", Version: "1.0.0"}); err != nil || info.License != "Apache-2.0" || info.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", info, err)
	}
	if d, err := l.PackageDetail(context.Background(), "acme-foo"); err != nil || d.Generic.LatestVersion != "1.2.0" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(context.Background(), dst, "acme-foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/v3/files/acme-foo-1.0.0.tar.gz", nil, nil); w.Code != 200 {
		t.Fatalf("promoted: %d", w.Code)
	}
	if w := do(l, http.MethodDelete, "/v3/releases/acme-foo-1.2.0", nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	w = do(l, http.MethodGet, "/v3/modules/acme-foo", nil, nil)
	if decode(t, w)["current_release"].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("after delete: %s", w.Body)
	}
	if err := l.DeleteVersion(context.Background(), "acme-foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/v3/modules/acme-foo", nil, nil); w.Code != 404 {
		t.Fatalf("gone: %d", w.Code)
	}
}

func TestPuppetRemote(t *testing.T) {
	up := newLocal(t, "up")
	tarball := moduleTarball(t, "acme-foo", "1.0.0")
	publish(t, up, tarball)
	publish(t, up, moduleTarball(t, "acme-foo", "2.0.0"))
	srv := httptest.NewServer(up)
	defer srv.Close()

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), newFS(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	w := do(rr, http.MethodGet, "/v3/releases?module=acme-foo", nil, nil)
	if w.Code != 200 || decode(t, w)["pagination"].(map[string]any)["total"].(float64) != 2 {
		t.Fatalf("releases: %d %s", w.Code, w.Body)
	}
	w = do(rr, http.MethodGet, "/v3/releases/acme-foo-1.0.0", nil, nil)
	if w.Code != 200 || decode(t, w)["file_uri"] != "/v3/files/acme-foo-1.0.0.tar.gz" {
		t.Fatalf("release: %d %s", w.Code, w.Body)
	}
	if w := do(rr, http.MethodGet, "/v3/files/acme-foo-1.0.0.tar.gz", nil, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tarball) {
		t.Fatalf("file: %d", w.Code)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/v3/files/acme-foo-1.0.0.tar.gz", nil, nil); w.Code != 200 {
		t.Fatalf("cached file: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/v3/modules/acme-foo", nil, nil); w.Code != 200 {
		t.Fatalf("cached module: %d %s", w.Code, w.Body)
	}
	if info, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "acme-foo", Version: "1.0.0"}); err != nil || info.License != "Apache-2.0" {
		t.Fatalf("info: %+v %v", info, err)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestPuppetVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	publish(t, a, moduleTarball(t, "acme-foo", "1.0.0"))
	publish(t, b, moduleTarball(t, "acme-foo", "2.0.0"))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	w := do(reg, http.MethodGet, "/v3/releases?module=acme-foo", nil, nil)
	if decode(t, w)["pagination"].(map[string]any)["total"].(float64) != 2 {
		t.Fatalf("merged: %s", w.Body)
	}
	w = do(reg, http.MethodGet, "/v3/modules/acme-foo", nil, nil)
	if decode(t, w)["current_release"].(map[string]any)["version"] != "2.0.0" {
		t.Fatalf("module: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/v3/files/acme-foo-1.0.0.tar.gz", nil, nil); w.Code != 200 {
		t.Fatalf("file: %d", w.Code)
	}
}
