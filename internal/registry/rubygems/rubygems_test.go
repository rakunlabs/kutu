package rubygems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
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

const specTmpl = `--- !ruby/object:Gem::Specification
name: NAME
version: !ruby/object:Gem::Version
  version: VERSION
platform: PLATFORM
authors:
- Jane Doe
autorequire:
bindir: bin
cert_chain: []
date: 2024-01-02 00:00:00.000000000 Z
dependencies:
- !ruby/object:Gem::Dependency
  name: rack
  requirement: !ruby/object:Gem::Requirement
    requirements:
    - - "~>"
      - !ruby/object:Gem::Version
        version: '2.0'
    - - ">="
      - !ruby/object:Gem::Version
        version: 2.0.1
  type: :runtime
  prerelease: false
  version_requirements: !ruby/object:Gem::Requirement
    requirements:
    - - "~>"
      - !ruby/object:Gem::Version
        version: '2.0'
- !ruby/object:Gem::Dependency
  name: rspec
  requirement: !ruby/object:Gem::Requirement
    requirements:
    - - ">="
      - !ruby/object:Gem::Version
        version: '0'
  type: :development
  prerelease: false
  version_requirements: !ruby/object:Gem::Requirement
    requirements:
    - - ">="
      - !ruby/object:Gem::Version
        version: '0'
description: A test gem
email:
- jane@example.com
executables: []
extensions: []
extra_rdoc_files: []
files:
- lib/NAME.rb
homepage: https://example.com/NAME
licenses:
- MIT
metadata:
  source_code_uri: https://example.com/src
post_install_message:
rdoc_options: []
require_paths:
- lib
required_ruby_version: !ruby/object:Gem::Requirement
  requirements:
  - - ">="
    - !ruby/object:Gem::Version
      version: 2.7.0
required_rubygems_version: !ruby/object:Gem::Requirement
  requirements:
  - - ">="
    - !ruby/object:Gem::Version
      version: '0'
requirements: []
rubygems_version: 3.4.10
signing_key:
specification_version: 4
summary: Test gem summary
test_files: []
`

func buildGem(t *testing.T, name, version, platform string) []byte {
	t.Helper()
	if platform == "" {
		platform = "ruby"
	}
	spec := strings.NewReplacer("NAME", name, "VERSION", version, "PLATFORM", platform).Replace(specTmpl)
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(b)
		_ = zw.Close()
		return buf.Bytes()
	}
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"metadata.gz", gz([]byte(spec))},
		{"data.tar.gz", gz([]byte("data"))},
		{"checksums.yaml.gz", gz([]byte("---\n"))},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o444, Size: int64(len(f.body))})
		_, _ = tw.Write(f.body)
	}
	_ = tw.Close()
	return out.Bytes()
}

func newLocal(t *testing.T, name string, policy *service.RegistryPolicy) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "gems", AllowPush: true, Policy: policy}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func push(t *testing.T, h http.Handler, gem []byte) {
	t.Helper()
	if w := do(h, http.MethodPost, "/api/v1/gems", gem); w.Code != http.StatusOK {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
}

func TestParseGem(t *testing.T) {
	m, err := parseGem(buildGem(t, "foo", "1.10", "x86_64-linux"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "foo" || m.Version != "1.10" || m.Platform != "x86_64-linux" {
		t.Fatalf("%+v", m)
	}
	if len(m.Dependencies) != 2 || m.Dependencies[0].Name != "rack" || strings.Join(m.Dependencies[0].Requirements, "&") != "~> 2.0&>= 2.0.1" {
		t.Fatalf("deps: %+v", m.Dependencies)
	}
	if m.Dependencies[1].Type != "development" {
		t.Fatalf("dev dep: %+v", m.Dependencies[1])
	}
	if strings.Join(m.RequiredRuby, ",") != ">= 2.7.0" || m.Licenses[0] != "MIT" || m.Homepage != "https://example.com/foo" || m.Authors[0] != "Jane Doe" {
		t.Fatalf("meta: %+v", m)
	}
	if m.Metadata["source_code_uri"] != "https://example.com/src" || m.RubygemsVersion != "3.4.10" {
		t.Fatalf("metadata: %+v", m)
	}
}

func TestLocalRoundTrip(t *testing.T) {
	l := newLocal(t, "gems", nil)
	push(t, l, buildGem(t, "foo", "1.0.0", ""))
	gem2 := buildGem(t, "foo", "1.1.0", "")
	push(t, l, gem2)
	push(t, l, buildGem(t, "foo", "1.1.0", "x86_64-linux"))
	push(t, l, buildGem(t, "bar", "0.1.0.pre", ""))

	w := do(l, http.MethodGet, "/info/foo", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != compactType || w.Header().Get("ETag") == "" || !strings.HasPrefix(w.Header().Get("Repr-Digest"), "sha-256=:") {
		t.Fatalf("info: %d %v", w.Code, w.Header())
	}
	info := w.Body.String()
	if !strings.HasPrefix(info, "---\n1.0.0 rack:~> 2.0&>= 2.0.1|checksum:") || !strings.Contains(info, ",ruby:>= 2.7.0\n") ||
		!strings.Contains(info, "\n1.1.0-x86_64-linux rack:") || strings.Contains(info, "rspec") || strings.Contains(info, "rubygems:") {
		t.Fatalf("info body:\n%s", info)
	}
	if w2 := do(l, http.MethodGet, "/info/foo", nil, "If-None-Match", w.Header().Get("ETag")); w2.Code != http.StatusNotModified {
		t.Fatalf("etag: %d", w2.Code)
	}

	versions := do(l, http.MethodGet, "/versions", nil).Body.String()
	if !strings.HasPrefix(versions, "created_at: ") || !strings.Contains(versions, "\n---\n") ||
		!strings.Contains(versions, "foo 1.0.0,1.1.0,1.1.0-x86_64-linux "+md5Hex([]byte(info))+"\n") ||
		!strings.Contains(versions, "bar 0.1.0.pre ") {
		t.Fatalf("versions:\n%s", versions)
	}
	if names := do(l, http.MethodGet, "/names", nil).Body.String(); names != "---\nbar\nfoo\n" {
		t.Fatalf("names: %q", names)
	}
	if w := do(l, http.MethodGet, "/gems/foo-1.1.0.gem", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), gem2) {
		t.Fatalf("download: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/gems/foo-1.1.0-x86_64-linux.gem", nil); w.Code != 200 {
		t.Fatalf("download platform: %d", w.Code)
	}

	// quick gemspec: zlib'd Marshal user-defined Gem::Specification.
	w = do(l, http.MethodGet, "/quick/Marshal.4.8/foo-1.0.0.gemspec.rz", nil)
	if w.Code != 200 {
		t.Fatalf("quick: %d", w.Code)
	}
	zr, err := zlib.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if !bytes.HasPrefix(raw, []byte{4, 8, 'u', ':'}) || !bytes.Contains(raw, []byte("Gem::Specification")) || !bytes.Contains(raw, []byte("Gem::Version")) {
		t.Fatalf("quick marshal: %q", raw)
	}

	// legacy indexes.
	gz := do(l, http.MethodGet, "/specs.4.8.gz", nil)
	zg, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	specs, _ := io.ReadAll(zg)
	if !bytes.HasPrefix(specs, []byte{4, 8, '[', 8}) || bytes.Contains(specs, []byte("bar")) {
		t.Fatalf("specs: %q", specs)
	}
	pre := do(l, http.MethodGet, "/prerelease_specs.4.8.gz", nil)
	zg, _ = gzip.NewReader(pre.Body)
	preb, _ := io.ReadAll(zg)
	if !bytes.Contains(preb, []byte("bar")) {
		t.Fatalf("prerelease specs: %q", preb)
	}

	// JSON API.
	if w := do(l, http.MethodGet, "/api/v1/gems/foo.json", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"1.1.0"`) || !strings.Contains(w.Body.String(), `"platform":"ruby"`) {
		t.Fatalf("gem json: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/api/v1/versions/foo.json", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"number":"1.0.0"`) {
		t.Fatalf("versions json: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/api/v1/dependencies.json?gems=foo,bar", nil); w.Code != 200 || strings.Count(w.Body.String(), `"name":`) != 4 {
		t.Fatalf("deps json: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/api/v1/dependencies?gems=foo", nil); w.Code != 200 || !bytes.HasPrefix(w.Body.Bytes(), []byte{4, 8, '['}) {
		t.Fatalf("deps marshal: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/api/v1/api_key", nil, "Authorization", "Bearer kutu_abc"); w.Body.String() != "kutu_abc" {
		t.Fatalf("api_key: %q", w.Body)
	}

	// policy hooks.
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/gems/foo-1.1.0-x86_64-linux.gem", nil)); !ok || ref.Name != "foo" || ref.Version != "1.1.0" {
		t.Fatalf("classify: %+v", ref)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/info/foo", nil)); !ok || ref.Name != "foo" || ref.Version != "" {
		t.Fatalf("classify info: %+v", ref)
	}
	if _, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/versions", nil)); ok {
		t.Fatal("classify versions should be false")
	}
	meta, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "foo", Version: "1.0.0"})
	if err != nil || meta.License != "MIT" || meta.PublishedAt.IsZero() {
		t.Fatalf("artifact info: %+v %v", meta, err)
	}
	d, err := l.PackageDetail(context.Background(), "foo")
	if err != nil || d.Generic.LatestVersion != "1.1.0" || d.Generic.License != "MIT" || len(d.Generic.Versions) != 2 || d.Generic.Versions[0].Version != "1.1.0" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}

	// yank (ruby platform only) then admin delete.
	form := url.Values{"gem_name": {"foo"}, "version": {"1.1.0"}}.Encode()
	if w := do(l, http.MethodDelete, "/api/v1/gems/yank", []byte(form)); w.Code != 200 {
		t.Fatalf("yank: %d %s", w.Code, w.Body)
	}
	info = do(l, http.MethodGet, "/info/foo", nil).Body.String()
	if strings.Contains(info, "\n1.1.0 ") || !strings.Contains(info, "1.1.0-x86_64-linux") {
		t.Fatalf("after yank:\n%s", info)
	}
	if w := do(l, http.MethodGet, "/gems/foo-1.1.0.gem", nil); w.Code != 404 {
		t.Fatalf("yanked download: %d", w.Code)
	}
	if err := l.DeleteVersion(context.Background(), "foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(context.Background(), "foo", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/info/foo", nil); w.Code != 404 {
		t.Fatalf("info after delete: %d", w.Code)
	}
	versions = do(l, http.MethodGet, "/versions", nil).Body.String()
	if strings.Contains(versions, "foo ") {
		t.Fatalf("versions after delete:\n%s", versions)
	}
	if err := l.DeleteVersion(context.Background(), "foo", "9.9"); err != registry.ErrPackageNotFound {
		t.Fatalf("missing delete: %v", err)
	}
}

func TestImmutableAndPromote(t *testing.T) {
	l := newLocal(t, "a", &service.RegistryPolicy{ImmutableVersions: true})
	gem := buildGem(t, "foo", "1.0.0", "")
	push(t, l, gem)
	if w := do(l, http.MethodPost, "/api/v1/gems", gem); w.Code != http.StatusConflict {
		t.Fatalf("immutable: %d", w.Code)
	}
	dst := newLocal(t, "b", nil)
	if err := l.PromoteVersion(context.Background(), dst, "foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/info/foo", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "1.0.0 ") {
		t.Fatalf("promoted info: %d %s", w.Code, w.Body)
	}
}

func TestMarshalEmpty(t *testing.T) {
	if got := rubyMarshal([]any{}); !bytes.Equal(got, []byte{4, 8, '[', 0}) {
		t.Fatalf("%x", got)
	}
	l := newLocal(t, "e", nil)
	zg, err := gzip.NewReader(do(l, http.MethodGet, "/latest_specs.4.8.gz", nil).Body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zg)
	if !bytes.Equal(b, []byte{4, 8, '[', 0}) {
		t.Fatalf("%x", b)
	}
	// [ "a", Gem::Version("1") , "ruby" ] round-trip shape.
	got := specsRaw([]*GemMeta{{Name: "a", Version: "1"}})
	want := []byte{4, 8, '[', 6, '[', 8,
		'I', '"', 6, 'a', 6, ':', 6, 'E', 'T',
		'U', ':', 17}
	want = append(want, "Gem::Version"...)
	want = append(want, '[', 6, 'I', '"', 6, '1', 6, ';', 0, 'T',
		'I', '"', 9, 'r', 'u', 'b', 'y', 6, ';', 0, 'T')
	if !bytes.Equal(got, want) {
		t.Fatalf("specs marshal\n got %q\nwant %q", got, want)
	}
}

func newRemote(t *testing.T, upstreamURL string) *Remote {
	t.Helper()
	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: upstreamURL}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return reg.(*Remote)
}

func TestRemote(t *testing.T) {
	up := newLocal(t, "up", nil)
	gem := buildGem(t, "foo", "1.0.0", "")
	push(t, up, gem)
	srv := httptest.NewServer(up)
	defer srv.Close()

	rr := newRemote(t, srv.URL)
	w := do(rr, http.MethodGet, "/versions", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "foo 1.0.0 ") || w.Header().Get("ETag") == "" || w.Header().Get("Repr-Digest") == "" {
		t.Fatalf("versions: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if w := do(rr, http.MethodGet, "/versions", nil, "Range", "bytes=5-"); w.Code != http.StatusPartialContent {
		t.Fatalf("range: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/info/foo", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "1.0.0 rack:") {
		t.Fatalf("info: %d %s", w.Code, w.Body)
	}
	if w := do(rr, http.MethodGet, "/gems/foo-1.0.0.gem", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), gem) {
		t.Fatalf("gem: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/api/v1/versions/foo.json", nil); w.Code != 200 {
		t.Fatalf("versions json: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/api/v1/gems/foo.json", nil, "X-Pika-Registry-Prefix", "/registries/default/rem"); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"gem_uri":"http://example.com/registries/default/rem/gems/foo-1.0.0.gem"`) {
		t.Fatalf("gem json: %d %s", w.Code, w.Body)
	}
	if err := rr.Prefetch(context.Background(), "foo", ""); err != nil {
		t.Fatal(err)
	}
	meta, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "foo", Version: "1.0.0"})
	if err != nil || meta.License != "MIT" || meta.PublishedAt.IsZero() {
		t.Fatalf("artifact info: %+v %v", meta, err)
	}
	d, err := rr.PackageDetail(context.Background(), "foo")
	if err != nil || d.Generic.LatestVersion != "1.0.0" || d.Generic.License != "MIT" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "foo" {
		t.Fatalf("list: %+v", pkgs)
	}
	if w := do(rr, http.MethodPost, "/api/v1/gems", gem); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote push: %d", w.Code)
	}

	srv.Close()
	if w := do(rr, http.MethodGet, "/gems/foo-1.0.0.gem", nil); w.Code != 200 {
		t.Fatalf("cached gem: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/info/foo", nil); w.Code != 200 {
		t.Fatalf("stale info: %d", w.Code)
	}
	st, _ := rr.PurgeCache(context.Background(), registry.PurgeOptions{})
	if st.PurgedFiles == 0 || !rr.store.Exists(gemRel("foo-1.0.0")) {
		t.Fatalf("purge: %+v", st)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVirtual(t *testing.T) {
	a := newLocal(t, "a", nil)
	b := newLocal(t, "b", nil)
	push(t, a, buildGem(t, "foo", "1.0.0", ""))
	push(t, b, buildGem(t, "foo", "1.0.0", ""))
	push(t, b, buildGem(t, "foo", "2.0.0", ""))
	push(t, b, buildGem(t, "bar", "0.1.0", ""))

	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	info := do(reg, http.MethodGet, "/info/foo", nil).Body.String()
	if strings.Count(info, "1.0.0 ") != 1 || !strings.Contains(info, "2.0.0 ") {
		t.Fatalf("merged info:\n%s", info)
	}
	versions := do(reg, http.MethodGet, "/versions", nil).Body.String()
	if !strings.Contains(versions, "foo 1.0.0,2.0.0 "+md5Hex([]byte(info))+"\n") || !strings.Contains(versions, "bar 0.1.0 ") {
		t.Fatalf("merged versions:\n%s", versions)
	}
	if names := do(reg, http.MethodGet, "/names", nil).Body.String(); names != "---\nbar\nfoo\n" {
		t.Fatalf("names: %q", names)
	}
	if w := do(reg, http.MethodGet, "/gems/foo-2.0.0.gem", nil); w.Code != 200 {
		t.Fatalf("first-hit gem: %d", w.Code)
	}
	if w := do(reg, http.MethodPost, "/api/v1/gems", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual push: %d", w.Code)
	}
	pkgs, _ := reg.(registry.PackageLister).ListPackages(context.Background())
	if len(pkgs) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
}
