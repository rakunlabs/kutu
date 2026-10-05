package conda

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

// bzFixture is bzpkg-0.5-py_0.tar.bz2 (noarch, MIT); stdlib cannot
// write bzip2 so it is generated once with `tar | bzip2`.
const bzFixture = "QlpoOTFBWSZTWVRW0G0AAMH/hMyAAEBQBf+QACIkCv//334AAIAIMADbMCVBEwBMhoDQBkAyPRPUEpNU08iaNAA00A2moDQACUlG1PU0yGjEYhkZNDINGhjyVOas96xQgFzBBEHlvszA6+oWMDSKEQMSAwUrFHRQovQnWokqctfZzpxWHxVYb1SgmpUF9eDFhiRCFp1gRCEuQB7CiRpHti0lL2rNG9pLLIyZAsSqyVS76lmmtFIxj6Lt4wW/ang9xulxXureL8RMRSirfzZYdc1BPDetOUHF+EihVeBkI/LydAQBTA2JEY+4o1GAuiSJrk5DIEyKhaqiBo4/mQ7nmYlAgP4u5IpwoSCoraDa"

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

func condaPkg(t *testing.T, name, version, build, subdir string) []byte {
	t.Helper()
	idx, _ := json.Marshal(map[string]any{
		"name": name, "version": version, "build": build, "build_number": 0,
		"depends": []string{"libc"}, "license": "Apache-2.0", "subdir": subdir, "timestamp": 1700000000000,
	})
	enc, _ := zstd.NewWriter(nil)
	info := enc.EncodeAll(tarOf(t, map[string]string{
		"info/index.json": string(idx),
		"info/about.json": `{"summary":"test pkg","home":"https://example.com"}`,
	}), nil)
	pkg := enc.EncodeAll(tarOf(t, map[string]string{"lib/x.so": "x"}), nil)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	stem := name + "-" + version + "-" + build
	for fn, body := range map[string][]byte{
		"metadata.json":             []byte(`{"conda_pkg_format_version":2}`),
		"info-" + stem + ".tar.zst": info,
		"pkg-" + stem + ".tar.zst":  pkg,
	} {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: fn, Method: zip.Store})
		_, _ = w.Write(body)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "conda", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
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

func repodata(t *testing.T, h http.Handler, subdir string) Repodata {
	t.Helper()
	w := do(h, http.MethodGet, "/"+subdir+"/repodata.json", nil)
	if w.Code != 200 {
		t.Fatalf("repodata %s: %d %s", subdir, w.Code, w.Body)
	}
	var rd Repodata
	if err := json.Unmarshal(w.Body.Bytes(), &rd); err != nil {
		t.Fatal(err)
	}
	return rd
}

func TestCondaLocal(t *testing.T) {
	l := newLocal(t, "c")
	ctx := context.Background()

	pkg := condaPkg(t, "foo", "1.0", "h123_0", "linux-64")
	if w := do(l, http.MethodPut, "/upload/osx-64", pkg); w.Code != http.StatusCreated {
		t.Fatalf("upload conda: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/linux-64/foo-1.1-h123_0.conda", condaPkg(t, "foo", "1.1", "h123_0", "linux-64")); w.Code != http.StatusCreated {
		t.Fatalf("upload conda path: %d %s", w.Code, w.Body)
	}
	bz, _ := base64.StdEncoding.DecodeString(bzFixture)
	if w := do(l, http.MethodPut, "/upload", bz); w.Code != http.StatusCreated {
		t.Fatalf("upload tar.bz2: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload", []byte("garbage")); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage: %d", w.Code)
	}

	rd := repodata(t, l, "linux-64")
	if len(rd.PackagesConda) != 2 || rd.Info["subdir"] != "linux-64" || rd.RepodataVersion != 1 {
		t.Fatalf("linux-64 repodata: %+v", rd)
	}
	var entry map[string]any
	_ = json.Unmarshal(rd.PackagesConda["foo-1.0-h123_0.conda"], &entry)
	if entry["sha256"] == "" || entry["md5"] == nil || entry["license"] != "Apache-2.0" || entry["size"].(float64) != float64(len(pkg)) {
		t.Fatalf("entry: %+v", entry)
	}
	if rd := repodata(t, l, "noarch"); len(rd.Packages) != 1 {
		t.Fatalf("noarch: %+v", rd)
	}
	if w := do(l, http.MethodGet, "/linux-64/current_repodata.json", nil); w.Code != 200 {
		t.Fatalf("current_repodata: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/linux-64/repodata.json.bz2", nil); w.Code != 404 {
		t.Fatalf("bz2: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/linux-64/repodata.json.zst", nil); w.Code != 200 {
		t.Fatalf("zst: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/win-64/repodata.json", nil); w.Code != 200 {
		t.Fatalf("empty subdir: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/linux-64/foo-1.0-h123_0.conda", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pkg) {
		t.Fatalf("download: %d", w.Code)
	}
	w := do(l, http.MethodGet, "/channeldata.json", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"bzpkg"`) || !strings.Contains(w.Body.String(), `"test pkg"`) {
		t.Fatalf("channeldata: %d %s", w.Code, w.Body)
	}

	pkgs, _ := l.ListPackages(ctx)
	if len(pkgs) != 2 || pkgs[1].Name != "foo" || len(pkgs[1].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(ctx, "foo")
	if err != nil || d.Generic.LatestVersion != "1.1" || d.Generic.License != "Apache-2.0" || d.Generic.Description != "test pkg" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/linux-64/foo-1.0-h123_0.conda", nil))
	if !ok || ref.Name != "foo" || ref.Version != "1.0" {
		t.Fatalf("classify: %+v %v", ref, ok)
	}
	meta, err := l.ArtifactInfo(ctx, registry.ArtifactRef{Name: "bzpkg", Version: "0.5"})
	if err != nil || meta.License != "MIT" || meta.PublishedAt.Year() != 2023 {
		t.Fatalf("info: %+v %v", meta, err)
	}

	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(ctx, dst, "foo", "1.0"); err != nil {
		t.Fatal(err)
	}
	if rd := repodata(t, dst, "linux-64"); len(rd.PackagesConda) != 1 {
		t.Fatalf("promoted: %+v", rd)
	}

	if err := l.DeleteVersion(ctx, "foo", "1.0"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(ctx, "foo", "1.0"); err != registry.ErrPackageNotFound {
		t.Fatalf("delete again: %v", err)
	}
	if rd := repodata(t, l, "linux-64"); len(rd.PackagesConda) != 1 {
		t.Fatalf("after delete: %+v", rd)
	}
	if w := do(l, http.MethodGet, "/linux-64/foo-1.0-h123_0.conda", nil); w.Code != 404 {
		t.Fatalf("download deleted: %d", w.Code)
	}
	if w := do(l, http.MethodDelete, "/noarch/bzpkg-0.5-py_0.tar.bz2", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete file: %d", w.Code)
	}
	if rd := repodata(t, l, "noarch"); len(rd.Packages) != 0 {
		t.Fatalf("noarch after delete: %+v", rd)
	}
}

func TestCondaRemoteAndVirtual(t *testing.T) {
	up := newLocal(t, "up")
	pkg := condaPkg(t, "bar", "2.0", "0", "linux-64")
	do(up, http.MethodPut, "/upload", pkg)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/linux-64/repodata.json" {
			// Upstream advertising a CEP-15 base_url must not leak to clients.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Replace(do(up, http.MethodGet, r.URL.Path, nil).Body.String(),
				`"info":{`, `"info":{"base_url":"https://upstream.example/x/",`, 1)))
			return
		}
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()

	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	w := do(rr, http.MethodGet, "/linux-64/repodata.json", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "bar-2.0-0.conda") || strings.Contains(w.Body.String(), "upstream.example") {
		t.Fatalf("remote repodata: %d %s", w.Code, w.Body)
	}
	if w := do(rr, http.MethodGet, "/linux-64/bar-2.0-0.conda", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pkg) {
		t.Fatalf("remote download: %d", w.Code)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "bar" {
		t.Fatalf("remote list: %+v", pkgs)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/linux-64/bar-2.0-0.conda", nil); w.Code != 200 {
		t.Fatalf("cached download: %d", w.Code)
	}
	if st, _ := rr.PurgeCache(context.Background(), registry.PurgeOptions{}); st.PurgedFiles != 1 {
		t.Fatalf("purge: %+v", st)
	}

	// Virtual over the local + remote (remote index still cached? no — purged; serve from local only).
	loc := newLocal(t, "loc")
	do(loc, http.MethodPut, "/upload", condaPkg(t, "baz", "1.0", "0", "linux-64"))
	loc2 := newLocal(t, "loc2")
	do(loc2, http.MethodPut, "/upload", condaPkg(t, "qux", "3.0", "0", "linux-64"))
	members := map[string]registry.Registry{"loc": loc, "loc2": loc2, "rem": rr}
	vreg, err := NewVirtualFactory(resolverFunc(func(_, n string) (registry.Registry, bool) {
		r, ok := members[n]
		return r, ok
	}))(context.Background(), registry.Deps{}, "default", &service.RegistryRepository{Name: "v", Members: []string{"loc", "rem", "loc2"}})
	if err != nil {
		t.Fatal(err)
	}
	rd := repodata(t, vreg, "linux-64")
	if len(rd.PackagesConda) != 2 {
		t.Fatalf("virtual repodata: %+v", rd)
	}
	if w := do(vreg, http.MethodGet, "/linux-64/qux-3.0-0.conda", nil); w.Code != 200 {
		t.Fatalf("virtual download: %d", w.Code)
	}
	if w := do(vreg, http.MethodGet, "/channeldata.json", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "qux") {
		t.Fatalf("virtual channeldata: %d %s", w.Code, w.Body)
	}
	if w := do(vreg, http.MethodPut, "/upload", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual put: %d", w.Code)
	}
}

type resolverFunc func(ns, name string) (registry.Registry, bool)

func (f resolverFunc) Lookup(ns, name string) (registry.Registry, bool) { return f(ns, name) }
