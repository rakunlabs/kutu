package cargo

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

func newNamedLocal(t *testing.T, name string, policy *service.RegistryPolicy) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "cargo", AllowPush: true, Policy: policy}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func req(h http.Handler, method, p string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/cargo-local")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func publishBody(t *testing.T, meta map[string]any, crate []byte) []byte {
	t.Helper()
	js, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(js)))
	b.Write(js)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(crate)))
	b.Write(crate)
	return b.Bytes()
}

func demoMeta(name, vers string) map[string]any {
	return map[string]any{
		"name": name, "vers": vers,
		"deps": []map[string]any{
			{"name": "serde", "version_req": "^1.0", "features": []string{"derive"}, "optional": false, "default_features": true, "target": nil, "kind": "normal", "registry": nil, "explicit_name_in_toml": nil},
			{"name": "rand_core", "version_req": "^0.6", "features": []string{}, "optional": true, "default_features": false, "target": "cfg(unix)", "kind": "dev", "registry": "https://github.com/rust-lang/crates.io-index", "explicit_name_in_toml": "rc"},
		},
		"features":     map[string][]string{"default": {"std"}, "std": {}, "extra": {"dep:rand_core", "serde?/std"}},
		"authors":      []string{"me"},
		"description":  "A demo crate for testing",
		"license":      "MIT OR Apache-2.0",
		"links":        nil,
		"rust_version": "1.70",
		"keywords":     []string{}, "categories": []string{},
	}
}

func readIndex(t *testing.T, h http.Handler, name string) []indexEntry {
	t.Helper()
	w := req(h, http.MethodGet, "/"+indexPath(name), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("index %s: %d %s", name, w.Code, w.Body)
	}
	return parseEntries(w.Body.Bytes())
}

func TestCargoPublishNew(t *testing.T) {
	l := newNamedLocal(t, "cargo-local", nil)
	crate := []byte("fake-crate-tarball")
	w := req(l, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("Demo_Crate", "1.2.0"), crate))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"invalid_categories":[]`) {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	ents := readIndex(t, l, "demo_crate")
	if len(ents) != 1 {
		t.Fatalf("entries: %+v", ents)
	}
	e := ents[0]
	if e.Name != "Demo_Crate" || e.Version != "1.2.0" || e.CKSum != pkgbase.SHA256Hex(crate) || e.RustVersion != "1.70" {
		t.Fatalf("entry: %+v", e)
	}
	if len(e.Deps) != 2 || e.Deps[0].Req != "^1.0" || e.Deps[0].Package != nil || e.Deps[0].Kind != "normal" {
		t.Fatalf("dep0: %+v", e.Deps)
	}
	d1 := e.Deps[1]
	if d1.Name != "rc" || d1.Package == nil || *d1.Package != "rand_core" || !d1.Optional || d1.DefaultFeatures || d1.Kind != "dev" || d1.Target == nil || *d1.Target != "cfg(unix)" || d1.Registry == nil {
		t.Fatalf("dep1: %+v", d1)
	}
	if e.V != 2 || len(e.Features2["extra"]) != 2 || len(e.Features["default"]) != 1 || e.Features["extra"] != nil {
		t.Fatalf("features: %+v / %+v v=%d", e.Features, e.Features2, e.V)
	}
	if w := req(l, http.MethodGet, "/api/v1/crates/Demo_Crate/1.2.0/download", nil); w.Code != 200 || w.Body.String() != string(crate) {
		t.Fatalf("download: %d %q", w.Code, w.Body)
	}
	// duplicate publish → 409 with cargo error envelope
	w = req(l, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("Demo_Crate", "1.2.0"), crate))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"errors":[{"detail"`) {
		t.Fatalf("dup: %d %s", w.Code, w.Body)
	}
	// malformed body
	if w := req(l, http.MethodPut, "/api/v1/crates/new", []byte{1, 2}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", w.Code)
	}
	info, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "demo_crate", Version: "1.2.0"})
	if err != nil || info.License != "MIT OR Apache-2.0" || info.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", info, err)
	}
	if _, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "demo_crate", Version: "9.9.9"}); err != registry.ErrPackageNotFound {
		t.Fatalf("info missing: %v", err)
	}
}

func TestCargoConfigAndETag(t *testing.T) {
	l := newNamedLocal(t, "cargo-local", nil)
	w := req(l, http.MethodGet, "/config.json", nil)
	var cfg map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &cfg)
	if cfg["auth-required"] != true || !strings.HasSuffix(cfg["dl"].(string), "/registries/default/cargo-local/api/v1/crates/{crate}/{version}/download") {
		t.Fatalf("config: %s", w.Body)
	}
	req(l, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("etag", "0.1.0"), []byte("x")))
	w = req(l, http.MethodGet, "/"+indexPath("etag"), nil)
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Last-Modified") == "" {
		t.Fatalf("validators missing: %v", w.Header())
	}
	if w := req(l, http.MethodGet, "/"+indexPath("etag"), nil, "If-None-Match", etag); w.Code != http.StatusNotModified {
		t.Fatalf("304: %d", w.Code)
	}
	if w := req(l, http.MethodGet, "/"+indexPath("missing"), nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing index: %d", w.Code)
	}
}

func TestCargoIndexSemverOrder(t *testing.T) {
	l := newNamedLocal(t, "cargo-local", nil)
	for _, v := range []string{"1.10.0", "1.2.0", "1.9.0-beta.1", "1.9.0"} {
		if w := req(l, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("ord", v), []byte(v))); w.Code != 200 {
			t.Fatalf("publish %s: %d", v, w.Code)
		}
	}
	var got []string
	for _, e := range readIndex(t, l, "ord") {
		got = append(got, e.Version)
	}
	if strings.Join(got, ",") != "1.2.0,1.9.0-beta.1,1.9.0,1.10.0" {
		t.Fatalf("order: %v", got)
	}
	d, _ := l.PackageDetail(context.Background(), "ord")
	if d.Cargo.LatestVersion != "1.10.0" {
		t.Fatalf("latest: %s", d.Cargo.LatestVersion)
	}
}

func TestCargoYankOwnersSearch(t *testing.T) {
	l := newNamedLocal(t, "cargo-local", nil)
	req(l, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("yanky", "1.0.0"), []byte("a")))
	req(l, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("yanky", "2.0.0"), []byte("b")))

	if w := req(l, http.MethodDelete, "/api/v1/crates/yanky/2.0.0/yank", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("yank: %d %s", w.Code, w.Body)
	}
	ents := readIndex(t, l, "yanky")
	if e, _ := findEntry(ents, "2.0.0"); !e.Yanked {
		t.Fatalf("not yanked: %+v", ents)
	}
	w := req(l, http.MethodGet, "/api/v1/crates?q=yan&per_page=5", nil)
	var sr searchResult
	_ = json.Unmarshal(w.Body.Bytes(), &sr)
	if sr.Meta.Total != 1 || sr.Crates[0].MaxVersion != "1.0.0" || sr.Crates[0].Description != "A demo crate for testing" {
		t.Fatalf("search: %s", w.Body)
	}
	if w := req(l, http.MethodPut, "/api/v1/crates/yanky/2.0.0/unyank", nil); w.Code != 200 {
		t.Fatalf("unyank: %d", w.Code)
	}
	if e, _ := findEntry(readIndex(t, l, "yanky"), "2.0.0"); e.Yanked {
		t.Fatalf("still yanked")
	}
	if w := req(l, http.MethodDelete, "/api/v1/crates/yanky/9.0.0/yank", nil); w.Code != http.StatusNotFound {
		t.Fatalf("yank missing: %d", w.Code)
	}
	if w := req(l, http.MethodGet, "/api/v1/crates?q=nomatch", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("empty search: %s", w.Body)
	}

	if w := req(l, http.MethodPut, "/api/v1/crates/yanky/owners", []byte(`{"users":["alice","bob"]}`)); w.Code != 200 || !strings.Contains(w.Body.String(), `"msg"`) {
		t.Fatalf("add owners: %d %s", w.Code, w.Body)
	}
	if w := req(l, http.MethodDelete, "/api/v1/crates/yanky/owners", []byte(`{"users":["alice"]}`)); w.Code != 200 {
		t.Fatalf("remove owners: %d", w.Code)
	}
	w = req(l, http.MethodGet, "/api/v1/crates/yanky/owners", nil)
	var ow struct {
		Users []owner `json:"users"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ow)
	if len(ow.Users) != 1 || ow.Users[0].Login != "bob" || ow.Users[0].ID == 0 {
		t.Fatalf("owners: %s", w.Body)
	}
	if w := req(l, http.MethodGet, "/api/v1/crates/nope/owners", nil); w.Code != http.StatusNotFound {
		t.Fatalf("owners missing crate: %d", w.Code)
	}
}

func TestCargoDeleteListClassifyPromote(t *testing.T) {
	ctx := context.Background()
	src := newNamedLocal(t, "src", nil)
	dst := newNamedLocal(t, "dst", nil)
	req(src, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("promo", "1.0.0"), []byte("one")))
	req(src, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("promo", "1.1.0"), []byte("two")))

	pkgs, _ := src.ListPackages(ctx)
	if len(pkgs) != 1 || pkgs[0].Name != "promo" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	if err := src.PromoteVersion(ctx, dst, "promo", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := src.PromoteVersion(ctx, dst, "promo", "3.0.0"); err != registry.ErrPackageNotFound {
		t.Fatalf("promote missing: %v", err)
	}
	ents := readIndex(t, dst, "promo")
	if len(ents) != 1 || ents[0].Version != "1.1.0" || len(ents[0].Deps) != 2 {
		t.Fatalf("dst index: %+v", ents)
	}
	if w := req(dst, http.MethodGet, "/api/v1/crates/promo/1.1.0/download", nil); w.Body.String() != "two" {
		t.Fatalf("dst download: %q", w.Body)
	}
	if info, _ := dst.ArtifactInfo(ctx, registry.ArtifactRef{Name: "promo", Version: "1.1.0"}); info.License == "" {
		t.Fatalf("dst meta not copied")
	}

	if err := src.DeleteVersion(ctx, "promo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := src.DeleteVersion(ctx, "promo", "1.0.0"); err != registry.ErrPackageNotFound {
		t.Fatalf("double delete: %v", err)
	}
	if ents := readIndex(t, src, "promo"); len(ents) != 1 || ents[0].Version != "1.1.0" {
		t.Fatalf("after delete: %+v", ents)
	}
	if err := src.DeleteVersion(ctx, "promo", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if w := req(src, http.MethodGet, "/"+indexPath("promo"), nil); w.Code != http.StatusNotFound {
		t.Fatalf("empty index should 404: %d", w.Code)
	}

	cases := map[string]registry.ArtifactRef{
		"/api/v1/crates/Promo/1.1.0/download": {Name: "promo", Version: "1.1.0"},
		"/pr/om/promo":                        {Name: "promo"},
		"/3/a/abc":                            {Name: "abc"},
		"/1/a":                                {Name: "a"},
	}
	for p, want := range cases {
		got, ok := src.ClassifyRequest(httptest.NewRequest(http.MethodGet, p, nil))
		if !ok || got != want {
			t.Fatalf("classify %s: %+v %v", p, got, ok)
		}
	}
	for _, p := range []string{"/config.json", "/api/v1/crates", "/xx/yy/promo"} {
		if _, ok := src.ClassifyRequest(httptest.NewRequest(http.MethodGet, p, nil)); ok {
			t.Fatalf("classify %s should be false", p)
		}
	}
}

func TestCargoImmutablePolicy(t *testing.T) {
	l := newNamedLocal(t, "cargo-local", &service.RegistryPolicy{ImmutableVersions: true})
	if w := req(l, http.MethodPut, "/api/v1/crates/imm/1.0.0/download", []byte("a")); w.Code != http.StatusCreated {
		t.Fatalf("raw put: %d", w.Code)
	}
	if w := req(l, http.MethodPut, "/api/v1/crates/imm/1.0.0/download", []byte("b")); w.Code != http.StatusConflict {
		t.Fatalf("immutable: %d %s", w.Code, w.Body)
	}
}

// fakeUpstream serves a sparse index + config.json with a custom dl.
type fakeUpstream struct {
	index      []byte
	crate      []byte
	indexHits  atomic.Int32
	notModHits atomic.Int32
	crateHits  atomic.Int32
}

func (f *fakeUpstream) handler(srvURL *string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/config.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"dl":"` + *srvURL + `/files/{lowerprefix}/{crate}/{crate}-{version}.crate?sum={sha256-checksum}"}`))
	})
	mux.HandleFunc("/up/st/upstr", func(w http.ResponseWriter, r *http.Request) {
		f.indexHits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			f.notModHits.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(f.index)
	})
	mux.HandleFunc("/files/up/st/upstr/upstr-1.0.0.crate", func(w http.ResponseWriter, r *http.Request) {
		f.crateHits.Add(1)
		if r.URL.Query().Get("sum") != pkgbase.SHA256Hex(f.crate) {
			http.Error(w, "bad sum", 400)
			return
		}
		_, _ = w.Write(f.crate)
	})
	return mux
}

func newRemote(t *testing.T, url, ttl string) *Remote {
	t.Helper()
	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: url, MutableTTL: ttl}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return reg.(*Remote)
}

func TestCargoRemote(t *testing.T) {
	crate := []byte("upstream-crate")
	f := &fakeUpstream{crate: crate}
	f.index = encodeEntries([]indexEntry{{Name: "upstr", Version: "1.0.0", CKSum: pkgbase.SHA256Hex(crate)}})
	var u string
	srv := httptest.NewServer(f.handler(&u))
	defer srv.Close()
	u = srv.URL

	rr := newRemote(t, srv.URL, "1ns")
	w := req(rr, http.MethodGet, "/config.json", nil)
	if strings.Contains(w.Body.String(), srv.URL) || !strings.Contains(w.Body.String(), "/registries/default/cargo-local/api/v1/crates") {
		t.Fatalf("config leaks upstream: %s", w.Body)
	}
	w = req(rr, http.MethodGet, "/up/st/upstr", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"vers":"1.0.0"`) {
		t.Fatalf("index: %d %s", w.Code, w.Body)
	}
	etag := w.Header().Get("ETag")
	// stale (ttl 1ns) → revalidated via If-None-Match → upstream 304
	w = req(rr, http.MethodGet, "/up/st/upstr", nil, "If-None-Match", etag)
	if w.Code != http.StatusNotModified || f.notModHits.Load() != 1 {
		t.Fatalf("revalidate: code=%d notmod=%d", w.Code, f.notModHits.Load())
	}
	w = req(rr, http.MethodGet, "/api/v1/crates/upstr/1.0.0/download", nil)
	if w.Code != 200 || w.Body.String() != string(crate) {
		t.Fatalf("download: %d %s", w.Code, w.Body)
	}
	req(rr, http.MethodGet, "/api/v1/crates/upstr/1.0.0/download", nil)
	if f.crateHits.Load() != 1 {
		t.Fatalf("crate not cached: %d", f.crateHits.Load())
	}
	if w := req(rr, http.MethodPut, "/api/v1/crates/new", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote write: %d", w.Code)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "upstr" {
		t.Fatalf("remote list: %+v", pkgs)
	}
	srv.Close()
	// upstream down → stale cache served
	if w := req(rr, http.MethodGet, "/up/st/upstr", nil); w.Code != 200 {
		t.Fatalf("stale fallback: %d", w.Code)
	}
}

func TestCargoRemotePrefetch(t *testing.T) {
	crate := []byte("upstream-crate")
	f := &fakeUpstream{crate: crate}
	f.index = encodeEntries([]indexEntry{{Name: "upstr", Version: "1.0.0", CKSum: pkgbase.SHA256Hex(crate)}, {Name: "upstr", Version: "2.0.0", Yanked: true}})
	var u string
	srv := httptest.NewServer(f.handler(&u))
	defer srv.Close()
	u = srv.URL
	rr := newRemote(t, srv.URL, "")
	if err := rr.Prefetch(context.Background(), "upstr", ""); err != nil {
		t.Fatal(err)
	}
	if !rr.store.Exists(crateRel("upstr", "1.0.0")) {
		t.Fatal("crate not prefetched")
	}
}

func TestExpandDL(t *testing.T) {
	if got := expandDL("https://static.crates.io/crates", "serde", "1.0.0", ""); got != "https://static.crates.io/crates/serde/1.0.0/download" {
		t.Fatal(got)
	}
	if got := expandDL("https://x/{prefix}/{lowerprefix}/{crate}-{version}/{sha256-checksum}", "AbCd", "1.0.0", "ff"); got != "https://x/Ab/Cd/ab/cd/AbCd-1.0.0/ff" {
		t.Fatal(got)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestCargoVirtualMerge(t *testing.T) {
	a := newNamedLocal(t, "a", nil)
	b := newNamedLocal(t, "b", nil)
	req(a, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("shared", "1.0.0"), []byte("a1")))
	req(b, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("shared", "1.0.0"), []byte("b1")))
	req(b, http.MethodPut, "/api/v1/crates/new", publishBody(t, demoMeta("shared", "2.0.0"), []byte("b2")))

	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default",
		&service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	v := reg.(*Virtual)
	ents := readIndex(t, v, "shared")
	if len(ents) != 2 || ents[0].CKSum != pkgbase.SHA256Hex([]byte("a1")) || ents[1].Version != "2.0.0" {
		t.Fatalf("merged: %+v", ents)
	}
	w := req(v, http.MethodGet, "/"+indexPath("shared"), nil)
	if w2 := req(v, http.MethodGet, "/"+indexPath("shared"), nil, "If-None-Match", w.Header().Get("ETag")); w2.Code != http.StatusNotModified {
		t.Fatalf("virtual 304: %d", w2.Code)
	}
	if w := req(v, http.MethodGet, "/api/v1/crates/shared/2.0.0/download", nil); w.Body.String() != "b2" {
		t.Fatalf("virtual download: %q", w.Body)
	}
	if w := req(v, http.MethodGet, "/config.json", nil); !strings.Contains(w.Body.String(), `"auth-required":true`) {
		t.Fatalf("virtual config: %s", w.Body)
	}
	if w := req(v, http.MethodGet, "/api/v1/crates?q=shared", nil); !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("virtual search: %s", w.Body)
	}
	if w := req(v, http.MethodPut, "/api/v1/crates/new", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual write: %d", w.Code)
	}
	pkgs, _ := v.ListPackages(context.Background())
	if len(pkgs) != 1 || len(pkgs[0].Versions) != 2 {
		t.Fatalf("virtual list: %+v", pkgs)
	}
}
