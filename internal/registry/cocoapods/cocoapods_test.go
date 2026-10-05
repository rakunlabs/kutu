package cocoapods

import (
	"archive/zip"
	"bytes"
	"context"
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
	repo := &service.RegistryRepository{Name: "pods", Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "pods", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/pods")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func spec(name, ver string) []byte {
	return []byte(`{"name":"` + name + `","version":"` + ver + `","license":"MIT","summary":"s","source":{"git":"https://example.com/x.git","tag":"` + ver + `"}}`)
}

func zipBytes(t *testing.T) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("Sources/a.swift")
	_, _ = f.Write([]byte("let a = 1"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestShard(t *testing.T) {
	// md5("AFNetworking") = a75d452377f3996bdc4b623a5df25820
	if got := SpecPath("AFNetworking", "4.0.1"); got != "Specs/a/7/5/AFNetworking/4.0.1/AFNetworking.podspec.json" {
		t.Fatal(got)
	}
}

func TestLocalPublishAndIndex(t *testing.T) {
	l := newLocal(t)
	for _, v := range []string{"1.0.0", "1.10.0"} {
		if w := do(l, http.MethodPut, "/pods/Foo/"+v, spec("Foo", v)); w.Code != http.StatusCreated {
			t.Fatalf("put: %d %s", w.Code, w.Body)
		}
	}
	if w := do(l, http.MethodGet, "/CocoaPods-version.yml", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "prefix_lengths") {
		t.Fatalf("yml: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/all_pods.txt", nil); w.Body.String() != "Foo\n" {
		t.Fatalf("all_pods: %q", w.Body)
	}
	shard := "/" + ShardFile("Foo")
	if w := do(l, http.MethodGet, shard, nil); w.Body.String() != "Foo/1.0.0/1.10.0\n" {
		t.Fatalf("shard: %q", w.Body)
	}
	w := do(l, http.MethodGet, "/"+SpecPath("Foo", "1.0.0"), nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"git":"https://example.com/x.git"`) {
		t.Fatalf("spec: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/pods/Foo/2.0.0", spec("Bar", "2.0.0")); w.Code != http.StatusBadRequest {
		t.Fatalf("mismatch: %d", w.Code)
	}
	d, err := l.PackageDetail(context.Background(), "Foo")
	if err != nil || d.Generic.LatestVersion != "1.10.0" || d.Generic.License != "MIT" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/"+SpecPath("Foo", "1.0.0"), nil)); !ok || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}
	if err := l.DeleteVersion(context.Background(), "Foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, shard, nil); w.Body.String() != "Foo/1.10.0\n" {
		t.Fatalf("shard after delete: %q", w.Body)
	}
	if err := l.DeleteVersion(context.Background(), "Foo", "1.10.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/all_pods.txt", nil); w.Body.String() != "" {
		t.Fatalf("all_pods after delete: %q", w.Body)
	}
}

func TestLocalMultipartZip(t *testing.T) {
	l := newLocal(t)
	z := zipBytes(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("podspec", string(spec("Zed", "0.1.0")))
	fw, _ := mw.CreateFormFile("file", "Zed.zip")
	_, _ = fw.Write(z)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPut, "/pods/Zed/0.1.0", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/pods")
	w := httptest.NewRecorder()
	l.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	w = do(l, http.MethodGet, "/"+SpecPath("Zed", "0.1.0"), nil)
	if !strings.Contains(w.Body.String(), `"http":"http://example.com/registries/default/pods/files/Zed/0.1.0.zip"`) {
		t.Fatalf("spec source: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/files/Zed/0.1.0.zip", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), z) {
		t.Fatalf("zip: %d", w.Code)
	}

	dst := newLocal(t)
	if err := l.PromoteVersion(context.Background(), dst, "Zed", "0.1.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/"+ShardFile("Zed"), nil); w.Body.String() != "Zed/0.1.0\n" {
		t.Fatalf("promoted shard: %q", w.Body)
	}
	if w := do(dst, http.MethodGet, "/files/Zed/0.1.0.zip", nil); w.Code != 200 {
		t.Fatalf("promoted zip: %d", w.Code)
	}
}

func TestRemote(t *testing.T) {
	up := newLocal(t)
	do(up, http.MethodPut, "/pods/Foo/1.0.0", spec("Foo", "1.0.0"))
	srv := httptest.NewServer(up)
	defer srv.Close()

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	if w := do(rr, http.MethodGet, "/"+ShardFile("Foo"), nil); w.Body.String() != "Foo/1.0.0\n" {
		t.Fatalf("shard: %d %q", w.Code, w.Body)
	}
	if err := rr.Prefetch(context.Background(), "Foo", ""); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/"+SpecPath("Foo", "1.0.0"), nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"Foo"`) {
		t.Fatalf("cached spec: %d %s", w.Code, w.Body)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "Foo" {
		t.Fatalf("list: %+v", pkgs)
	}
}

type stubResolver map[string]registry.Registry

func (s stubResolver) Lookup(_, repo string) (registry.Registry, bool) {
	r, ok := s[repo]
	return r, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t), newLocal(t)
	do(a, http.MethodPut, "/pods/Foo/1.0.0", spec("Foo", "1.0.0"))
	do(b, http.MethodPut, "/pods/Foo/2.0.0", spec("Foo", "2.0.0"))
	do(b, http.MethodPut, "/pods/Bar/1.0.0", spec("Bar", "1.0.0"))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(stubResolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(reg, http.MethodGet, "/"+ShardFile("Foo"), nil); !strings.Contains(w.Body.String(), "Foo/1.0.0/2.0.0\n") {
		t.Fatalf("shard: %q", w.Body)
	}
	if w := do(reg, http.MethodGet, "/all_pods.txt", nil); w.Body.String() != "Bar\nFoo\n" {
		t.Fatalf("all_pods: %q", w.Body)
	}
	if w := do(reg, http.MethodGet, "/"+SpecPath("Foo", "2.0.0"), nil); w.Code != 200 {
		t.Fatalf("spec: %d", w.Code)
	}
	if w := do(reg, http.MethodPut, "/pods/Foo/3.0.0", spec("Foo", "3.0.0")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual put: %d", w.Code)
	}
}
