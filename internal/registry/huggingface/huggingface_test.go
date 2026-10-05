package huggingface

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func depsFor(t *testing.T) registry.Deps {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
}

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "hf", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), depsFor(t), "default", repo)
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

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestParseRoute(t *testing.T) {
	cases := []struct {
		in   string
		op   int
		id   string
		kind string
		rev  string
		file string
	}{
		{"/api/models/org/m", opInfo, "org/m", kindModels, "", ""},
		{"/api/models/gpt2/revision/main", opInfo, "gpt2", kindModels, "main", ""},
		{"/api/datasets/org/d/revision/refs%2Fpr%2F1", opInfo, "org/d", kindDatasets, "refs/pr/1", ""},
		{"/api/models/org/m/tree/main/sub/dir", opTree, "org/m", kindModels, "main", "sub/dir"},
		{"/org/m/resolve/main/a/b.json", opResolve, "org/m", kindModels, "main", "a/b.json"},
		{"/gpt2/resolve/main/config.json", opResolve, "gpt2", kindModels, "main", "config.json"},
		{"/datasets/org/d/resolve/v1/data.parquet", opResolve, "org/d", kindDatasets, "v1", "data.parquet"},
	}
	for _, c := range cases {
		rt, ok := parseRoute(c.in)
		if !ok || rt.op != c.op || rt.ref.ID != c.id || rt.ref.Kind != c.kind || rt.rev != c.rev || rt.file != c.file {
			t.Errorf("%s: %+v %v", c.in, rt, ok)
		}
	}
	if _, ok := parseRoute("/org/m/resolve/main/../x"); ok {
		t.Error("traversal accepted")
	}
}

func TestLocalRoundTrip(t *testing.T) {
	l := newLocal(t, "hf")
	if w := do(l, http.MethodPut, "/org/m/resolve/main/config.json", []byte(`{"a":1}`)); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	readme := []byte("---\nlicense: apache-2.0\n---\n# model\n")
	w := do(l, http.MethodPut, "/org/m/resolve/main/README.md", readme)
	if w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	var put struct{ Commit string }
	_ = json.Unmarshal(w.Body.Bytes(), &put)

	w = do(l, http.MethodGet, "/api/models/org/m", nil)
	var info upstreamInfo
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil || info.SHA != put.Commit || len(info.Siblings) != 2 {
		t.Fatalf("info: %d %s", w.Code, w.Body)
	}
	w = do(l, http.MethodHead, "/org/m/resolve/main/config.json", nil)
	if w.Code != 200 || w.Header().Get("X-Repo-Commit") != put.Commit || w.Header().Get("ETag") != `"`+gitBlobID([]byte(`{"a":1}`))+`"` || w.Header().Get("Content-Length") != "7" {
		t.Fatalf("head: %d %v", w.Code, w.Header())
	}
	if w := do(l, http.MethodGet, "/org/m/resolve/"+put.Commit+"/config.json", nil); w.Code != 200 || w.Body.String() != `{"a":1}` {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/api/models/org/m/tree/main", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "README.md") {
		t.Fatalf("tree: %d %s", w.Code, w.Body)
	}
	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "org/m" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(context.Background(), "org/m")
	if err != nil || d.Generic.LatestVersion != put.Commit || d.Generic.License != "apache-2.0" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if err := l.DeleteVersion(context.Background(), "org/m", put.Commit); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/org/m/resolve/main/README.md", nil); w.Code != 404 {
		t.Fatalf("after delete: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/org/m/resolve/main/config.json", nil); w.Code != 200 {
		t.Fatalf("parent: %d", w.Code)
	}

	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(context.Background(), dst, "org/m", "main"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/org/m/resolve/main/config.json", nil); w.Code != 200 {
		t.Fatalf("promoted: %d", w.Code)
	}
}

const commit = "0123456789abcdef0123456789abcdef01234567"

type fakeHub struct {
	cfg, model []byte
	fileHits   int
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/models/org/m", "/api/models/org/m/revision/main", "/api/models/org/m/revision/" + commit:
		if r.URL.Query().Get("blobs") != "true" {
			http.Error(w, "need blobs", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "org/m", "sha": commit, "cardData": map[string]any{"license": "mit"},
			"siblings": []map[string]any{
				{"rfilename": "config.json", "size": len(h.cfg), "blobId": gitBlobID(h.cfg)},
				{"rfilename": "model.bin", "size": len(h.model), "blobId": "x", "lfs": map[string]any{"sha256": sha256hex(h.model), "size": len(h.model)}},
			},
		})
	case "/org/m/resolve/" + commit + "/config.json":
		h.fileHits++
		_, _ = w.Write(h.cfg)
	case "/org/m/resolve/" + commit + "/model.bin":
		http.Redirect(w, r, "/cdn/blob", http.StatusFound)
	case "/cdn/blob":
		h.fileHits++
		_, _ = w.Write(h.model)
	default:
		http.NotFound(w, r)
	}
}

func TestRemote(t *testing.T) {
	hub := &fakeHub{cfg: []byte(`{"x":true}`), model: bytes.Repeat([]byte("w"), 4096)}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), depsFor(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)

	w := do(rr, http.MethodHead, "/org/m/resolve/main/model.bin", nil)
	h := w.Header()
	if w.Code != 200 || h.Get("X-Repo-Commit") != commit || h.Get("ETag") != `"`+sha256hex(hub.model)+`"` ||
		h.Get("X-Linked-Etag") != `"`+sha256hex(hub.model)+`"` || h.Get("X-Linked-Size") != "4096" || h.Get("Content-Length") != "4096" || h.Get("Location") != "" {
		t.Fatalf("head: %d %v", w.Code, h)
	}
	if w := do(rr, http.MethodGet, "/org/m/resolve/main/model.bin", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), hub.model) {
		t.Fatalf("get model: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/org/m/resolve/"+commit+"/config.json", nil); w.Code != 200 || w.Body.String() != string(hub.cfg) ||
		w.Header().Get("ETag") != `"`+gitBlobID(hub.cfg)+`"` {
		t.Fatalf("get cfg: %d %v", w.Code, w.Header())
	}
	if hub.fileHits != 2 {
		t.Fatalf("file hits: %d", hub.fileHits)
	}
	srv.Close()

	if w := do(rr, http.MethodGet, "/org/m/resolve/main/model.bin", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), hub.model) {
		t.Fatalf("cached model: %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest(http.MethodGet, "/org/m/resolve/"+commit+"/model.bin", nil)
	r.Header.Set("Range", "bytes=0-9")
	w = httptest.NewRecorder()
	rr.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.Len() != 10 {
		t.Fatalf("range: %d %d", w.Code, w.Body.Len())
	}
	if w := do(rr, http.MethodGet, "/api/models/org/m/revision/main", nil); w.Code != 200 || !strings.Contains(w.Body.String(), commit) {
		t.Fatalf("cached info: %d", w.Code)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "org/m" || len(pkgs[0].Versions) != 1 || pkgs[0].Versions[0] != commit {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := rr.PackageDetail(context.Background(), "org/m")
	if err != nil || d.Generic.License != "mit" {
		t.Fatalf("detail: %+v %v", d, err)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	do(b, http.MethodPut, "/org/m/resolve/main/f.txt", []byte("from-b"))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(reg, http.MethodGet, "/org/m/resolve/main/f.txt", nil); w.Code != 200 || w.Body.String() != "from-b" {
		t.Fatalf("virtual: %d %s", w.Code, w.Body)
	}
	if w := do(reg, http.MethodGet, "/org/x/resolve/main/f.txt", nil); w.Code != 404 {
		t.Fatalf("virtual miss: %d", w.Code)
	}
}
