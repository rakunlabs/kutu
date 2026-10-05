package ansible

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

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func collection(t *testing.T, ns, name, version string) []byte {
	manifest := map[string]any{"collection_info": map[string]any{
		"namespace": ns, "name": name, "version": version, "license": []string{"MIT"},
		"description": "test collection", "dependencies": map[string]string{"community.general": ">=1.0.0"},
	}}
	b, _ := json.Marshal(manifest)
	return tarGz(t, map[string]string{"MANIFEST.json": string(b), "meta/runtime.yml": "requires_ansible: '>=2.15'\n"})
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
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/gal")
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
	fw, _ := mw.CreateFormFile("file", "x.tar.gz")
	_, _ = fw.Write(tarball)
	_ = mw.Close()
	return do(h, http.MethodPost, "/api/v3/artifacts/collections/", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return m
}

func TestAnsibleLocal(t *testing.T) {
	l := newLocal(t, "gal")
	if w := do(l, http.MethodGet, "/api/", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"v3":"v3/"`) {
		t.Fatalf("root: %d %s", w.Code, w.Body)
	}
	w := publish(t, l, collection(t, "acme", "tools", "1.0.0"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	task := decode(t, w)["task"].(string)
	if !strings.HasPrefix(task, "http://example.com/registries/default/gal/api/v3/imports/collections/") {
		t.Fatalf("task: %s", task)
	}
	if w := do(l, http.MethodGet, strings.TrimPrefix(task, "http://example.com/registries/default/gal"), nil, nil); w.Code != 200 || decode(t, w)["state"] != "completed" {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	if w := publish(t, l, collection(t, "acme", "tools", "1.10.0")); w.Code != http.StatusAccepted {
		t.Fatalf("publish2: %d %s", w.Code, w.Body)
	}

	w = do(l, http.MethodGet, "/api/v3/collections/acme/tools/", nil, nil)
	if w.Code != 200 || decode(t, w)["highest_version"].(map[string]any)["version"] != "1.10.0" {
		t.Fatalf("collection: %d %s", w.Code, w.Body)
	}
	w = do(l, http.MethodGet, "/api/v3/plugin/ansible/content/published/collections/index/acme/tools/versions/?limit=1", nil, nil)
	vl := decode(t, w)
	if w.Code != 200 || vl["meta"].(map[string]any)["count"].(float64) != 2 || vl["links"].(map[string]any)["next"] == nil {
		t.Fatalf("versions: %d %s", w.Code, w.Body)
	}
	if v := vl["data"].([]any)[0].(map[string]any)["version"]; v != "1.10.0" {
		t.Fatalf("newest first: %v", v)
	}
	w = do(l, http.MethodGet, "/api/v3/collections/acme/tools/versions/1.0.0/", nil, nil)
	vd := decode(t, w)
	dl := vd["download_url"].(string)
	if dl != "http://example.com/registries/default/gal/download/acme-tools-1.0.0.tar.gz" {
		t.Fatalf("download_url: %s", dl)
	}
	if deps := vd["metadata"].(map[string]any)["dependencies"].(map[string]any); deps["community.general"] != ">=1.0.0" {
		t.Fatalf("deps: %v", deps)
	}
	if w := do(l, http.MethodGet, "/download/acme-tools-1.0.0.tar.gz", nil, nil); w.Code != 200 || w.Body.Len() == 0 {
		t.Fatalf("download: %d", w.Code)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/download/acme-tools-1.0.0.tar.gz", nil)); !ok || ref.Name != "acme.tools" || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}
	if info, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "acme.tools", Version: "1.0.0"}); err != nil || info.License != "MIT" || info.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", info, err)
	}
	d, err := l.PackageDetail(context.Background(), "acme.tools")
	if err != nil || d.Generic.LatestVersion != "1.10.0" || len(d.Generic.Versions) != 2 {
		t.Fatalf("detail: %+v %v", d, err)
	}

	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(context.Background(), dst, "acme.tools", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/api/v3/collections/acme/tools/versions/1.0.0/", nil, nil); w.Code != 200 {
		t.Fatalf("promoted: %d", w.Code)
	}

	if w := do(l, http.MethodDelete, "/api/v3/collections/acme/tools/versions/1.10.0/", nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	w = do(l, http.MethodGet, "/api/v3/collections/acme/tools/", nil, nil)
	if decode(t, w)["highest_version"].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("after delete: %s", w.Body)
	}
	if err := l.DeleteVersion(context.Background(), "acme.tools", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/api/v3/collections/acme/tools/", nil, nil); w.Code != 404 {
		t.Fatalf("gone: %d", w.Code)
	}
}

// fakeGalaxy mimics galaxy.ansible.com: classic paths 404, content paths serve.
func fakeGalaxy(t *testing.T, tarball []byte) *httptest.Server {
	const base = "/api/v3/plugin/ansible/content/published/collections/index/acme/tools/"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case base + "versions/":
			if r.URL.Query().Get("offset") == "1" {
				_, _ = io.WriteString(w, `{"meta":{"count":2},"links":{"next":null},"data":[{"version":"1.0.0"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"meta":{"count":2},"links":{"next":"`+base+`versions/?limit=1&offset=1"},"data":[{"version":"2.0.0"}]}`)
		case base + "versions/1.0.0/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": "1.0.0", "download_url": srv.URL + "/api/v3/plugin/ansible/content/published/collections/artifacts/acme-tools-1.0.0.tar.gz",
				"artifact":   map[string]any{"filename": "acme-tools-1.0.0.tar.gz", "sha256": sha(tarball), "size": len(tarball)},
				"metadata":   map[string]any{"dependencies": map[string]any{}, "license": []string{"GPL-3.0"}},
				"created_at": "2024-01-01T00:00:00Z",
			})
		case "/api/v3/plugin/ansible/content/published/collections/artifacts/acme-tools-1.0.0.tar.gz":
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func sha(b []byte) string { return pkgbase.SHA256Hex(b) }

func newRemote(t *testing.T, url string) *Remote {
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: url}
	reg, err := NewRemoteFactory()(context.Background(), newFS(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return reg.(*Remote)
}

func TestAnsibleRemote(t *testing.T) {
	tarball := collection(t, "acme", "tools", "1.0.0")
	srv := fakeGalaxy(t, tarball)
	defer srv.Close()
	rr := newRemote(t, srv.URL)

	w := do(rr, http.MethodGet, "/api/v3/collections/acme/tools/versions/", nil, nil)
	if w.Code != 200 || decode(t, w)["meta"].(map[string]any)["count"].(float64) != 2 {
		t.Fatalf("versions: %d %s", w.Code, w.Body)
	}
	w = do(rr, http.MethodGet, "/api/v3/collections/acme/tools/versions/1.0.0/", nil, nil)
	if w.Code != 200 {
		t.Fatalf("version: %d %s", w.Code, w.Body)
	}
	dl := decode(t, w)["download_url"].(string)
	if !strings.HasPrefix(dl, "http://example.com/registries/default/gal/download/") {
		t.Fatalf("download_url not rewritten: %s", dl)
	}
	if w := do(rr, http.MethodGet, "/download/acme-tools-1.0.0.tar.gz", nil, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tarball) {
		t.Fatalf("download: %d", w.Code)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/download/acme-tools-1.0.0.tar.gz", nil, nil); w.Code != 200 {
		t.Fatalf("cached download: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/api/v3/collections/acme/tools/versions/", nil, nil); w.Code != 200 {
		t.Fatalf("stale versions: %d", w.Code)
	}
	if info, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "acme.tools", Version: "1.0.0"}); err != nil || info.License != "GPL-3.0" {
		t.Fatalf("info: %+v %v", info, err)
	}
	if pkgs, _ := rr.ListPackages(context.Background()); len(pkgs) != 1 {
		t.Fatalf("list: %+v", pkgs)
	}
	if w := do(rr, http.MethodPost, "/api/v3/artifacts/collections/", nil, nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("write: %d", w.Code)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestAnsibleVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	publish(t, a, collection(t, "acme", "tools", "1.0.0"))
	publish(t, b, collection(t, "acme", "tools", "2.0.0"))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	w := do(reg, http.MethodGet, "/api/v3/collections/acme/tools/versions/", nil, nil)
	if w.Code != 200 || decode(t, w)["meta"].(map[string]any)["count"].(float64) != 2 {
		t.Fatalf("merged versions: %d %s", w.Code, w.Body)
	}
	w = do(reg, http.MethodGet, "/api/v3/collections/acme/tools/", nil, nil)
	if decode(t, w)["highest_version"].(map[string]any)["version"] != "2.0.0" {
		t.Fatalf("collection: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/download/acme-tools-2.0.0.tar.gz", nil, nil); w.Code != 200 {
		t.Fatalf("download: %d", w.Code)
	}
	if w := do(reg, http.MethodGet, "/api/v3/collections/acme/tools/versions/1.0.0/", nil, nil); w.Code != 200 {
		t.Fatalf("version: %d", w.Code)
	}
}
