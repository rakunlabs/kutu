package apt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/klauspost/compress/zstd"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func arOf(members ...[2]any) []byte {
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	for _, m := range members {
		name, body := m[0].(string), m[1].([]byte)
		fmt.Fprintf(&buf, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(body))
		buf.Write(body)
		if len(body)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// buildDeb builds a minimal .deb. compression is "gz", "zst" or "".
func buildDeb(t *testing.T, name, version, arch, compression string) []byte {
	t.Helper()
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <t@example.com>\nDepends: libc6 (>= 2.31)\nDescription: test package %s\n multi-line body\n .\n second paragraph\n", name, version, arch, name)
	ctl := tarOf(t, map[string]string{"./control": control, "./md5sums": ""})
	ctlName := "control.tar"
	switch compression {
	case "gz":
		ctl, ctlName = gz(ctl), "control.tar.gz"
	case "zst":
		enc, _ := zstd.NewWriter(nil)
		ctl, ctlName = enc.EncodeAll(ctl, nil), "control.tar.zst"
	}
	data := gz(tarOf(t, map[string]string{"./usr/bin/" + name: "#!/bin/sh\necho " + version + "\n"}))
	return arOf([2]any{"debian-binary", []byte("2.0\n")}, [2]any{ctlName, ctl}, [2]any{"data.tar.gz", data})
}

func testKey(t *testing.T) (string, openpgp.EntityList) {
	t.Helper()
	e, err := openpgp.NewEntity("kutu apt test", "", "apt@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	return buf.String(), openpgp.EntityList{e}
}

func testDeps(t *testing.T) registry.Deps {
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
}

func newLocal(t *testing.T, name, key string, policy *service.RegistryPolicy) *Local {
	t.Helper()
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "apt/" + name, AllowPush: true, SigningKey: key, Policy: policy}
	r, err := NewLocalFactory()(context.Background(), testDeps(t), "default", repo)
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

func mustGet(t *testing.T, h http.Handler, p string) []byte {
	t.Helper()
	w := do(h, http.MethodGet, p, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", p, w.Code, w.Body)
	}
	return w.Body.Bytes()
}

func verifyRelease(t *testing.T, h http.Handler, dist string) {
	t.Helper()
	release := mustGet(t, h, "/dists/"+dist+"/Release")
	for path, want := range releaseFiles(release) {
		got := mustGet(t, h, "/dists/"+dist+"/"+path)
		if sha := sha256hex(got); sha != want {
			t.Fatalf("%s: sha256 %s != release %s", path, sha, want)
		}
	}
}

func sha256hex(b []byte) string { return pkgbase.SHA256Hex(b) }

func TestParseDebCompressions(t *testing.T) {
	for _, c := range []string{"gz", "zst", ""} {
		ctl, err := parseDeb(buildDeb(t, "hello", "1:1.0-1", "amd64", c))
		if err != nil {
			t.Fatalf("%q: %v", c, err)
		}
		if ctl.Get("Package") != "hello" || ctl.Get("Version") != "1:1.0-1" || !strings.Contains(ctl.Get("Description"), "\n multi-line body") {
			t.Fatalf("%q: %+v", c, ctl)
		}
	}
	if _, err := parseDeb([]byte("nope")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLocalPublishSignDelete(t *testing.T) {
	armored, ring := testKey(t)
	l := newLocal(t, "deb", armored, nil)

	if w := do(l, http.MethodPut, "/upload/stable/main", buildDeb(t, "hello", "1.0-1", "amd64", "gz")); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload", buildDeb(t, "hello", "1.1-1", "amd64", "zst")); w.Code != http.StatusCreated {
		t.Fatalf("upload default: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPost, "/api/upload?distribution=stable&component=main", buildDeb(t, "libfoo-data", "2.0", "all", "")); w.Code != http.StatusCreated {
		t.Fatalf("api upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload/bookworm/contrib", buildDeb(t, "hello", "1.0-1", "arm64", "gz")); w.Code != http.StatusCreated {
		t.Fatalf("upload bookworm: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload/stable/main", []byte("garbage")); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage: %d", w.Code)
	}

	amd := string(mustGet(t, l, "/dists/stable/main/binary-amd64/Packages"))
	for _, want := range []string{
		"Package: hello\nVersion: 1.0-1\nArchitecture: amd64\n",
		"Version: 1.1-1",
		"Package: libfoo-data",
		"Filename: pool/main/h/hello/hello_1.0-1_amd64.deb",
		"Filename: pool/main/libf/libfoo-data/libfoo-data_2.0_all.deb",
		"Description: test package hello\n multi-line body\n .\n second paragraph\nFilename:",
		"SHA256: ", "MD5sum: ", "SHA1: ", "Size: ",
	} {
		if !strings.Contains(amd, want) {
			t.Fatalf("Packages missing %q:\n%s", want, amd)
		}
	}
	arm := string(mustGet(t, l, "/dists/stable/main/binary-arm64/Packages"))
	if !strings.Contains(arm, "libfoo-data") || strings.Contains(arm, "Package: hello") {
		t.Fatalf("arm64 Packages wrong:\n%s", arm)
	}
	gzBody := mustGet(t, l, "/dists/stable/main/binary-amd64/Packages.gz")
	zr, _ := gzip.NewReader(bytes.NewReader(gzBody))
	if raw, _ := io.ReadAll(zr); string(raw) != amd {
		t.Fatal("Packages.gz differs from Packages")
	}

	release := mustGet(t, l, "/dists/stable/Release")
	for _, want := range []string{"Suite: stable", "Codename: stable", "Architectures: amd64 arm64", "Components: main", "MD5Sum:", "SHA1:", "SHA256:", "main/binary-amd64/Packages.gz"} {
		if !strings.Contains(string(release), want) {
			t.Fatalf("Release missing %q:\n%s", want, release)
		}
	}
	verifyRelease(t, l, "stable")
	verifyRelease(t, l, "bookworm")

	in := mustGet(t, l, "/dists/stable/InRelease")
	blk, _ := clearsign.Decode(in)
	if blk == nil {
		t.Fatal("InRelease is not clearsigned")
	}
	if _, err := blk.VerifySignature(ring, nil); err != nil {
		t.Fatalf("InRelease verify: %v", err)
	}
	if !bytes.Equal(bytes.TrimRight(blk.Plaintext, "\n"), bytes.TrimRight(release, "\n")) {
		t.Fatal("InRelease plaintext differs from Release")
	}
	sig := mustGet(t, l, "/dists/stable/Release.gpg")
	if _, err := openpgp.CheckArmoredDetachedSignature(ring, bytes.NewReader(release), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("Release.gpg verify: %v", err)
	}
	pub := mustGet(t, l, "/public.key")
	if _, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pub)); err != nil {
		t.Fatalf("public key: %v", err)
	}

	deb := mustGet(t, l, "/pool/main/h/hello/hello_1.0-1_amd64.deb")
	if _, err := parseDeb(deb); err != nil {
		t.Fatal(err)
	}

	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 2 || pkgs[0].Name != "hello" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(context.Background(), "hello")
	if err != nil || d.Generic.LatestVersion != "1.1-1" || d.Generic.Description != "test package hello" || len(d.Generic.Versions) != 2 {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/pool/main/h/hello/hello_1.0-1_amd64.deb", nil)); !ok || ref.Name != "hello" || ref.Version != "1.0-1" {
		t.Fatalf("classify: %+v %v", ref, ok)
	}
	if meta, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "hello", Version: "1.0-1"}); err != nil || meta.PublishedAt.IsZero() {
		t.Fatalf("artifact info: %+v %v", meta, err)
	}

	// Delete through the admin interface removes it from every dist.
	if err := l.DeleteVersion(context.Background(), "hello", "1.0-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(context.Background(), "hello", "1.0-1"); err != registry.ErrPackageNotFound {
		t.Fatalf("second delete: %v", err)
	}
	amd = string(mustGet(t, l, "/dists/stable/main/binary-amd64/Packages"))
	if strings.Contains(amd, "Version: 1.0-1") || !strings.Contains(amd, "Version: 1.1-1") {
		t.Fatalf("after delete:\n%s", amd)
	}
	if w := do(l, http.MethodGet, "/pool/main/h/hello/hello_1.0-1_amd64.deb", nil); w.Code != http.StatusNotFound {
		t.Fatalf("pool after delete: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/dists/bookworm/Release", nil); w.Code != http.StatusNotFound {
		t.Fatalf("empty dist should vanish: %d", w.Code)
	}
	verifyRelease(t, l, "stable")
	in = mustGet(t, l, "/dists/stable/InRelease")
	if blk, _ := clearsign.Decode(in); blk == nil {
		t.Fatal("InRelease missing after delete")
	} else if _, err := blk.VerifySignature(ring, nil); err != nil {
		t.Fatal(err)
	}

	// DELETE via pool path.
	if w := do(l, http.MethodDelete, "/pool/main/h/hello/hello_1.1-1_amd64.deb", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete pool: %d %s", w.Code, w.Body)
	}
	if amd := string(mustGet(t, l, "/dists/stable/main/binary-amd64/Packages")); strings.Contains(amd, "Package: hello") {
		t.Fatalf("hello still listed:\n%s", amd)
	}
}

func TestLocalUnsignedAndPolicies(t *testing.T) {
	l := newLocal(t, "u", "", &service.RegistryPolicy{ImmutableVersions: true})
	deb := buildDeb(t, "ab", "1", "amd64", "gz")
	if w := do(l, http.MethodPut, "/upload/stable/main", deb); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload/stable/main", deb); w.Code != http.StatusConflict {
		t.Fatalf("immutable: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/dists/stable/InRelease", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unsigned InRelease: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/public.key", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unsigned key: %d", w.Code)
	}
	verifyRelease(t, l, "stable")
	if w := do(l, http.MethodPut, "/upload/../x/main", deb); w.Code != http.StatusBadRequest {
		t.Fatalf("bad dist: %d", w.Code)
	}
}

func TestPromote(t *testing.T) {
	src := newLocal(t, "src", "", nil)
	dst := newLocal(t, "dst", "", nil)
	do(src, http.MethodPut, "/upload/stable/main", buildDeb(t, "tool", "3.0", "amd64", "gz"))
	if err := src.PromoteVersion(context.Background(), dst, "tool", "3.0"); err != nil {
		t.Fatal(err)
	}
	if amd := string(mustGet(t, dst, "/dists/stable/main/binary-amd64/Packages")); !strings.Contains(amd, "Package: tool") {
		t.Fatalf("promoted Packages:\n%s", amd)
	}
	mustGet(t, dst, "/pool/main/t/tool/tool_3.0_amd64.deb")
	verifyRelease(t, dst, "stable")
}

func TestRemote(t *testing.T) {
	up := newLocal(t, "up", "", nil)
	do(up, http.MethodPut, "/upload/stable/main", buildDeb(t, "hello", "1.0", "amd64", "gz"))
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL, MutableTTL: "1h"}
	reg, err := NewRemoteFactory()(context.Background(), testDeps(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	upRelease := mustGet(t, up, "/dists/stable/Release")
	if got := mustGet(t, rr, "/dists/stable/Release"); !bytes.Equal(got, upRelease) {
		t.Fatal("remote Release differs from upstream")
	}
	verifyRelease(t, rr, "stable")
	deb := mustGet(t, rr, "/pool/main/h/hello/hello_1.0_amd64.deb")
	if !bytes.Equal(deb, mustGet(t, up, "/pool/main/h/hello/hello_1.0_amd64.deb")) {
		t.Fatal("deb mismatch")
	}
	if err := rr.Prefetch(context.Background(), "hello", ""); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "hello" {
		t.Fatalf("remote list: %+v", pkgs)
	}
	if d, err := rr.PackageDetail(context.Background(), "hello"); err != nil || d.Generic.LatestVersion != "1.0" {
		t.Fatalf("remote detail: %+v %v", d, err)
	}

	srv.Close()
	mustGet(t, rr, "/pool/main/h/hello/hello_1.0_amd64.deb")
	mustGet(t, rr, "/dists/stable/main/binary-amd64/Packages.gz")
	if w := do(rr, http.MethodPut, "/upload/stable/main", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote write: %d", w.Code)
	}
	st, _ := rr.PurgeCache(context.Background(), registry.PurgeOptions{})
	if st.PurgedFiles == 0 {
		t.Fatal("purge removed nothing")
	}
	mustGet(t, rr, "/pool/main/h/hello/hello_1.0_amd64.deb")
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVirtual(t *testing.T) {
	a := newLocal(t, "a", "", nil)
	armored, _ := testKey(t)
	b := newLocal(t, "b", armored, nil)
	do(a, http.MethodPut, "/upload/stable/main", buildDeb(t, "alpha", "1.0", "amd64", "gz"))
	do(b, http.MethodPut, "/upload/stable/main", buildDeb(t, "beta", "2.0", "arm64", "gz"))
	do(b, http.MethodPut, "/upload/stable/extra", buildDeb(t, "gamma", "1.0", "all", "gz"))

	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	v := reg.(*Virtual)
	release := string(mustGet(t, v, "/dists/stable/Release"))
	if !strings.Contains(release, "Components: extra main") || !strings.Contains(release, "Architectures: amd64 arm64") {
		t.Fatalf("virtual Release:\n%s", release)
	}
	verifyRelease(t, v, "stable")
	if amd := string(mustGet(t, v, "/dists/stable/main/binary-amd64/Packages")); !strings.Contains(amd, "Package: alpha") || strings.Contains(amd, "Package: beta") {
		t.Fatalf("virtual amd64:\n%s", amd)
	}
	if arm := string(mustGet(t, v, "/dists/stable/main/binary-arm64/Packages")); !strings.Contains(arm, "Package: beta") {
		t.Fatalf("virtual arm64:\n%s", arm)
	}
	if ex := string(mustGet(t, v, "/dists/stable/extra/binary-amd64/Packages")); !strings.Contains(ex, "Package: gamma") {
		t.Fatalf("virtual extra:\n%s", ex)
	}
	if w := do(v, http.MethodGet, "/dists/stable/InRelease", nil); w.Code != http.StatusNotFound {
		t.Fatalf("virtual InRelease: %d", w.Code)
	}
	mustGet(t, v, "/pool/main/b/beta/beta_2.0_arm64.deb")
	mustGet(t, v, "/pool/main/a/alpha/alpha_1.0_amd64.deb")
	pkgs, _ := v.ListPackages(context.Background())
	if len(pkgs) != 3 {
		t.Fatalf("virtual list: %+v", pkgs)
	}
	if w := do(v, http.MethodPut, "/upload/stable/main", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual write: %d", w.Code)
	}
}
