package chef

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
	"github.com/rakunlabs/kutu/internal/service"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func cookbookJSON(t *testing.T, name, version string) []byte {
	meta, _ := json.Marshal(map[string]any{
		"name": name, "version": version, "license": "Apache-2.0", "description": "test cookbook",
		"maintainer": "acme", "dependencies": map[string]string{"apt": ">= 2.0"},
	})
	return tarGz(t, map[string]string{name + "/metadata.json": string(meta), name + "/recipes/default.rb": "# noop\n"})
}

func cookbookRB(t *testing.T, name, version string) []byte {
	rb := "name '" + name + "'\nmaintainer 'acme'\nlicense 'MIT'\nversion '" + version + "'\ndepends 'apt'\ndepends \"yum\", \"~> 3.0\"\n"
	return tarGz(t, map[string]string{name + "/metadata.rb": rb})
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

const prefix = "/registries/default/chef"

func do(h http.Handler, method, p string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, body)
	r.Header.Set("X-Pika-Registry-Prefix", prefix)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func publishMultipart(t *testing.T, h http.Handler, tarball []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("cookbook", `{"category":"Utilities"}`)
	fw, _ := mw.CreateFormFile("tarball", "cb.tgz")
	_, _ = fw.Write(tarball)
	_ = mw.Close()
	return do(h, http.MethodPost, "/api/v1/cookbooks", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return m
}

func TestChefLocal(t *testing.T) {
	l := newLocal(t, "chef")
	if w := publishMultipart(t, l, cookbookJSON(t, "nginx", "1.0.0")); w.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/api/v1/cookbooks/nginx/1.2.0", bytes.NewReader(cookbookRB(t, "nginx", "1.2.0")), nil); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/api/v1/cookbooks/nginx/9.9.9", bytes.NewReader(cookbookRB(t, "nginx", "1.3.0")), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("mismatch: %d", w.Code)
	}

	w := do(l, http.MethodGet, "/universe", nil, nil)
	var uni map[string]map[string]map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &uni)
	e := uni["nginx"]["1.2.0"]
	base := "http://example.com" + prefix
	if e["location_path"] != base+"/api/v1" || e["download_url"] != base+"/api/v1/cookbooks/nginx/versions/1.2.0/download" {
		t.Fatalf("universe: %s", w.Body)
	}
	if deps := e["dependencies"].(map[string]any); deps["apt"] != ">= 0.0.0" || deps["yum"] != "~> 3.0" {
		t.Fatalf("rb deps: %v", deps)
	}

	w = do(l, http.MethodGet, "/api/v1/cookbooks/nginx", nil, nil)
	cb := decode(t, w)
	if cb["latest_version"] != base+"/api/v1/cookbooks/nginx/versions/1.2.0" || len(cb["versions"].([]any)) != 2 {
		t.Fatalf("cookbook: %s", w.Body)
	}
	w = do(l, http.MethodGet, "/api/v1/cookbooks/nginx/versions/1_0_0", nil, nil)
	if v := decode(t, w); v["license"] != "Apache-2.0" || v["file"] != base+"/api/v1/cookbooks/nginx/versions/1.0.0/download" {
		t.Fatalf("version: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/api/v1/cookbooks/nginx/versions/1.0.0/download", nil, nil); w.Code != 200 || w.Body.Len() == 0 {
		t.Fatalf("download: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/api/v1/search?q=ngi", nil, nil); decode(t, w)["total"].(float64) != 1 {
		t.Fatalf("search: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/api/v1/cookbooks?start=0&items=5", nil, nil); decode(t, w)["total"].(float64) != 1 {
		t.Fatalf("list: %s", w.Body)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/api/v1/cookbooks/nginx/versions/1.0.0/download", nil)); !ok || ref.Name != "nginx" || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}
	if info, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "nginx", Version: "1.2.0"}); err != nil || info.License != "MIT" {
		t.Fatalf("info: %+v %v", info, err)
	}
	if d, err := l.PackageDetail(context.Background(), "nginx"); err != nil || d.Generic.LatestVersion != "1.2.0" || len(d.Generic.Versions) != 2 {
		t.Fatalf("detail: %+v %v", d, err)
	}
	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(context.Background(), dst, "nginx", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/universe", nil, nil); !strings.Contains(w.Body.String(), `"1.0.0"`) {
		t.Fatalf("promoted: %s", w.Body)
	}
	if w := do(l, http.MethodDelete, "/api/v1/cookbooks/nginx/versions/1.2.0", nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/universe", nil, nil); strings.Contains(w.Body.String(), "1.2.0") {
		t.Fatalf("universe after delete: %s", w.Body)
	}
	if err := l.DeleteVersion(context.Background(), "nginx", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/api/v1/cookbooks/nginx", nil, nil); w.Code != 404 {
		t.Fatalf("gone: %d", w.Code)
	}
}

func TestChefRemote(t *testing.T) {
	up := newLocal(t, "up")
	tarball := cookbookJSON(t, "nginx", "1.0.0")
	publishMultipart(t, up, tarball)
	srv := httptest.NewServer(up)
	defer srv.Close()

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), newFS(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	base := "http://example.com" + prefix
	w := do(rr, http.MethodGet, "/universe", nil, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), srv.URL) || !strings.Contains(w.Body.String(), base+"/api/v1/cookbooks/nginx/versions/1.0.0/download") {
		t.Fatalf("universe: %d %s", w.Code, w.Body)
	}
	w = do(rr, http.MethodGet, "/api/v1/cookbooks/nginx", nil, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), srv.URL) {
		t.Fatalf("cookbook: %d %s", w.Code, w.Body)
	}
	w = do(rr, http.MethodGet, "/api/v1/search?q=nginx", nil, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), srv.URL) || !strings.Contains(w.Body.String(), base+"/api/v1/cookbooks/nginx") {
		t.Fatalf("search: %d %s", w.Code, w.Body)
	}
	if w := do(rr, http.MethodGet, "/api/v1/cookbooks/nginx/versions/1.0.0/download", nil, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tarball) {
		t.Fatalf("download: %d", w.Code)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/api/v1/cookbooks/nginx/versions/1.0.0/download", nil, nil); w.Code != 200 {
		t.Fatalf("cached download: %d", w.Code)
	}
	if info, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "nginx", Version: "1.0.0"}); err != nil || info.License != "Apache-2.0" {
		t.Fatalf("info: %+v %v", info, err)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestChefVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	publishMultipart(t, a, cookbookJSON(t, "nginx", "1.0.0"))
	publishMultipart(t, b, cookbookJSON(t, "nginx", "2.0.0"))
	publishMultipart(t, b, cookbookJSON(t, "redis", "0.1.0"))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	w := do(reg, http.MethodGet, "/universe", nil, nil)
	var uni map[string]map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &uni)
	if len(uni["nginx"]) != 2 || len(uni["redis"]) != 1 {
		t.Fatalf("merged universe: %s", w.Body)
	}
	w = do(reg, http.MethodGet, "/api/v1/cookbooks/nginx", nil, nil)
	if len(decode(t, w)["versions"].([]any)) != 2 {
		t.Fatalf("cookbook: %s", w.Body)
	}
	if w := do(reg, http.MethodGet, "/api/v1/cookbooks/nginx/versions/2.0.0/download", nil, nil); w.Code != 200 {
		t.Fatalf("download: %d", w.Code)
	}
}
