package pub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "pub", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func makeArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"pubspec.yaml": "name: " + name + "\nversion: " + version + "\ndescription: test pkg\nenvironment:\n  sdk: '>=3.0.0 <4.0.0'\n",
		"lib/a.dart":   "void main() {}\n",
	}
	for n, c := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c))})
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func do(h http.Handler, method, p string, body []byte, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("Accept", contentType)
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func publish(t *testing.T, h http.Handler, archive []byte) {
	t.Helper()
	w := do(h, http.MethodGet, "/api/packages/versions/new", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/api/packages/versions/newUpload") {
		t.Fatalf("new: %d %s", w.Code, w.Body)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "package.tar.gz")
	_, _ = fw.Write(archive)
	_ = mw.Close()
	w = do(h, http.MethodPost, "/api/packages/versions/newUpload", body.Bytes(), mw.FormDataContentType())
	if w.Code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	loc := w.Header().Get("Location")
	i := strings.Index(loc, "/api/packages/versions/newUploadFinish")
	if i < 0 {
		t.Fatalf("location: %q", loc)
	}
	w = do(h, http.MethodGet, loc[i:], nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success"`) {
		t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
}

func getDoc(t *testing.T, h http.Handler, pkg string) packageDoc {
	t.Helper()
	w := do(h, http.MethodGet, "/api/packages/"+pkg, nil, "")
	if w.Code != 200 {
		t.Fatalf("doc %s: %d %s", pkg, w.Code, w.Body)
	}
	var d packageDoc
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPubLocalRoundTrip(t *testing.T) {
	l := newLocal(t, "p")
	a1 := makeArchive(t, "foo", "1.0.0")
	publish(t, l, a1)
	publish(t, l, makeArchive(t, "foo", "1.2.0"))
	publish(t, l, makeArchive(t, "foo", "2.0.0-dev.1"))

	d := getDoc(t, l, "foo")
	if len(d.Versions) != 3 || d.Latest == nil || d.Latest.Version != "1.2.0" {
		t.Fatalf("doc: %+v", d)
	}
	if !strings.HasPrefix(d.Versions[0].ArchiveURL, "http://example.com/packages/foo/versions/") {
		t.Fatalf("archive url: %s", d.Versions[0].ArchiveURL)
	}
	if !strings.Contains(string(d.Latest.Pubspec), `"description":"test pkg"`) {
		t.Fatalf("pubspec: %s", d.Latest.Pubspec)
	}
	w := do(l, http.MethodGet, "/packages/foo/versions/1.0.0.tar.gz", nil, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), a1) {
		t.Fatalf("download: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/api/packages/foo/versions/1.2.0", nil, ""); w.Code != 200 {
		t.Fatalf("version: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/api/search?q=fo", nil, ""); !strings.Contains(w.Body.String(), `"foo"`) {
		t.Fatalf("search: %s", w.Body)
	}

	// retract latest → latest falls back
	if w := do(l, http.MethodPost, "/api/packages/foo/versions/1.2.0/retract", nil, ""); w.Code != 200 {
		t.Fatalf("retract: %d %s", w.Code, w.Body)
	}
	if d := getDoc(t, l, "foo"); d.Latest.Version != "1.0.0" {
		t.Fatalf("latest after retract: %s", d.Latest.Version)
	}

	pd, err := l.PackageDetail(context.Background(), "foo")
	if err != nil || pd.Generic.Versions[0].Version != "2.0.0-dev.1" || pd.Generic.Description != "test pkg" {
		t.Fatalf("detail: %+v %v", pd, err)
	}
	if ai, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "foo", Version: "1.0.0"}); err != nil || ai.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", ai, err)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest("GET", "/packages/foo/versions/1.0.0.tar.gz", nil)); !ok || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}

	if err := l.DeleteVersion(context.Background(), "foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if d := getDoc(t, l, "foo"); len(d.Versions) != 2 || d.find("1.0.0") != nil {
		t.Fatalf("after delete: %+v", d.Versions)
	}
	if w := do(l, http.MethodGet, "/packages/foo/versions/1.0.0.tar.gz", nil, ""); w.Code != 404 {
		t.Fatalf("deleted download: %d", w.Code)
	}

	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(context.Background(), dst, "foo", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if d := getDoc(t, dst, "foo"); len(d.Versions) != 1 {
		t.Fatalf("promoted: %+v", d)
	}
}

func TestPubInvalidUpload(t *testing.T) {
	l := newLocal(t, "p")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "package.tar.gz")
	_, _ = fw.Write([]byte("not a tarball"))
	_ = mw.Close()
	w := do(l, http.MethodPost, "/api/packages/versions/newUpload", body.Bytes(), mw.FormDataContentType())
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"InvalidInput"`) {
		t.Fatalf("invalid: %d %s", w.Code, w.Body)
	}
}

func TestPubRemote(t *testing.T) {
	archive := makeArchive(t, "bar", "0.1.0")
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/packages/bar":
			d := packageDoc{Name: "bar", Versions: []versionEntry{{
				Version: "0.1.0", ArchiveURL: upstream.URL + "/blobs/bar-0.1.0.tar.gz",
				ArchiveSHA256: sha(archive), Pubspec: json.RawMessage(`{"name":"bar","version":"0.1.0"}`),
				Published: "2024-01-02T03:04:05Z",
			}}}
			d.finish()
			_ = json.NewEncoder(w).Encode(d)
		case "/blobs/bar-0.1.0.tar.gz":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: upstream.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	d := getDoc(t, rr, "bar")
	if d.Latest == nil || d.Latest.ArchiveURL != "http://example.com/packages/bar/versions/0.1.0.tar.gz" {
		t.Fatalf("rewrite: %+v", d.Latest)
	}
	if w := do(rr, http.MethodGet, "/packages/bar/versions/0.1.0.tar.gz", nil, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), archive) {
		t.Fatalf("archive: %d", w.Code)
	}
	upstream.Close()
	if w := do(rr, http.MethodGet, "/packages/bar/versions/0.1.0.tar.gz", nil, ""); w.Code != 200 {
		t.Fatalf("cached archive: %d", w.Code)
	}
	if ai, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "bar", Version: "0.1.0"}); err != nil || ai.PublishedAt.Year() != 2024 {
		t.Fatalf("info: %+v %v", ai, err)
	}
	if pkgs, _ := rr.ListPackages(context.Background()); len(pkgs) != 1 || pkgs[0].Name != "bar" {
		t.Fatalf("list: %+v", pkgs)
	}
	if w := do(rr, http.MethodPost, "/api/packages/versions/newUpload", nil, ""); w.Code != 405 {
		t.Fatalf("remote write: %d", w.Code)
	}
}

func sha(b []byte) string { return pkgbase.SHA256Hex(b) }

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestPubVirtualMerge(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	publish(t, a, makeArchive(t, "baz", "1.0.0"))
	publish(t, b, makeArchive(t, "baz", "1.1.0"))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	d := getDoc(t, reg, "baz")
	if len(d.Versions) != 2 || d.Latest.Version != "1.1.0" {
		t.Fatalf("merged: %+v", d)
	}
	if w := do(reg, http.MethodGet, "/packages/baz/versions/1.1.0.tar.gz", nil, ""); w.Code != 200 {
		t.Fatalf("virtual archive: %d", w.Code)
	}
	if w := do(reg, http.MethodGet, "/api/packages/versions/new", nil, ""); w.Code != 405 {
		t.Fatalf("virtual publish: %d", w.Code)
	}
}
