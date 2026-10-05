package terraform

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

func testKey(t *testing.T) (string, *openpgp.Entity) {
	t.Helper()
	e, err := openpgp.NewEntity("kutu test", "", "test@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.String(), e
}

func armoredPublic(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.Bytes()
}

func newLocal(t *testing.T, name, key string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "tf", AllowPush: true, SigningKey: key}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
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

func do(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Host = "kutu.test"
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

// localPath strips scheme+host from a URL returned by kutu.
func localPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

func tarGz(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(content))
	gz.Close()
	return buf.Bytes()
}

func zipOf(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create(name)
	_, _ = f.Write([]byte(content))
	zw.Close()
	return buf.Bytes()
}

type downloadDoc struct {
	Protocols           []string `json:"protocols"`
	OS                  string   `json:"os"`
	Arch                string   `json:"arch"`
	Filename            string   `json:"filename"`
	DownloadURL         string   `json:"download_url"`
	SHASumsURL          string   `json:"shasums_url"`
	SHASumsSignatureURL string   `json:"shasums_signature_url"`
	SHASum              string   `json:"shasum"`
	SigningKeys         struct {
		GPGPublicKeys []struct {
			KeyID      string `json:"key_id"`
			ASCIIArmor string `json:"ascii_armor"`
		} `json:"gpg_public_keys"`
	} `json:"signing_keys"`
}

func getDownload(t *testing.T, h http.Handler, p string) downloadDoc {
	t.Helper()
	var d downloadDoc
	if err := json.Unmarshal(mustGet(t, h, p), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func verify(t *testing.T, armoredKey string, sums, sig []byte) {
	t.Helper()
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckDetachedSignature(ring, bytes.NewReader(sums), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("signature: %v", err)
	}
}

func TestWellKnown(t *testing.T) {
	l := newLocal(t, "l", "")
	r := httptest.NewRequest(http.MethodGet, "/.well-known/terraform.json", nil)
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/tf")
	w := httptest.NewRecorder()
	l.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"modules.v1":"/registries/default/tf/v1/modules/"`) {
		t.Fatalf("well-known: %s", w.Body)
	}
	if string(WellKnown("")) != `{"modules.v1":"/v1/modules/","providers.v1":"/v1/providers/"}` {
		t.Fatalf("WellKnown: %s", WellKnown(""))
	}
}

func TestLocalModules(t *testing.T) {
	l := newLocal(t, "l", "")
	tgz := tarGz(t, "main.tf")
	for _, v := range []string{"1.0.0", "1.10.0"} {
		if w := do(l, http.MethodPut, "/v1/modules/acme/vpc/aws/"+v, tgz); w.Code != http.StatusCreated {
			t.Fatalf("publish %s: %d %s", v, w.Code, w.Body)
		}
	}
	if w := do(l, http.MethodPut, "/v1/modules/acme/vpc/aws/2.0.0", []byte("not an archive")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad archive: %d", w.Code)
	}
	zipBody := zipOf(t, "main.tf", "x")
	if w := do(l, http.MethodPut, "/v1/modules/acme/dns/gcp/0.1.0", zipBody); w.Code != http.StatusCreated {
		t.Fatalf("publish zip: %d %s", w.Code, w.Body)
	}

	vs := string(mustGet(t, l, "/v1/modules/acme/vpc/aws/versions"))
	if !strings.Contains(vs, `{"version":"1.10.0"},{"version":"1.0.0"}`) {
		t.Fatalf("versions: %s", vs)
	}
	if latest := string(mustGet(t, l, "/v1/modules/acme/vpc/aws")); !strings.Contains(latest, `"version":"1.10.0"`) {
		t.Fatalf("latest: %s", latest)
	}
	w := do(l, http.MethodGet, "/v1/modules/acme/vpc/aws/1.0.0/download", nil)
	get := w.Header().Get("X-Terraform-Get")
	if w.Code != http.StatusNoContent || get != "http://kutu.test/archive/modules/acme/vpc/aws/1.0.0.tar.gz" {
		t.Fatalf("download: %d %q", w.Code, get)
	}
	if b := mustGet(t, l, localPath(t, get)); !bytes.Equal(b, tgz) {
		t.Fatal("archive mismatch")
	}
	w = do(l, http.MethodGet, "/v1/modules/acme/dns/gcp/0.1.0/download", nil)
	if get := w.Header().Get("X-Terraform-Get"); !strings.HasSuffix(get, "/0.1.0.zip") {
		t.Fatalf("zip download: %q", get)
	}
	if list := string(mustGet(t, l, "/v1/modules?q=vpc")); !strings.Contains(list, `"name":"vpc"`) || strings.Contains(list, `"name":"dns"`) {
		t.Fatalf("search: %s", list)
	}

	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 2 || pkgs[1].Name != "modules/acme/vpc/aws" || len(pkgs[1].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(context.Background(), "modules/acme/vpc/aws")
	if err != nil || d.Generic.LatestVersion != "1.10.0" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/v1/modules/acme/vpc/aws/1.0.0/download", nil)); !ok || ref.Name != "modules/acme/vpc/aws" || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}
	if err := l.DeleteVersion(context.Background(), "modules/acme/vpc/aws", "1.10.0"); err != nil {
		t.Fatal(err)
	}
	if vs := string(mustGet(t, l, "/v1/modules/acme/vpc/aws/versions")); strings.Contains(vs, "1.10.0") {
		t.Fatalf("after delete: %s", vs)
	}
	if err := l.DeleteVersion(context.Background(), "modules/acme/vpc/aws", "9.9.9"); err != registry.ErrPackageNotFound {
		t.Fatalf("delete missing: %v", err)
	}
}

func publishProvider(t *testing.T, l *Local) (amd, arm []byte) {
	t.Helper()
	amd = zipOf(t, "terraform-provider-foo_v1.0.0", "amd")
	arm = zipOf(t, "terraform-provider-foo_v1.0.0", "arm")
	if w := do(l, http.MethodPut, "/v1/providers/acme/foo/1.0.0/linux/amd64?protocols=5.0,6.0", amd); w.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/v1/providers/acme/foo/1.0.0/darwin/arm64", arm); w.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	return amd, arm
}

func TestLocalProvidersSelfSigned(t *testing.T) {
	armored, _ := testKey(t)
	l := newLocal(t, "l", armored)
	amd, arm := publishProvider(t, l)

	var vs struct {
		Versions []struct {
			Version   string              `json:"version"`
			Protocols []string            `json:"protocols"`
			Platforms []map[string]string `json:"platforms"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(mustGet(t, l, "/v1/providers/acme/foo/versions"), &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs.Versions) != 1 || vs.Versions[0].Version != "1.0.0" || len(vs.Versions[0].Platforms) != 2 || strings.Join(vs.Versions[0].Protocols, ",") != "5.0,6.0" {
		t.Fatalf("versions: %+v", vs)
	}

	d := getDownload(t, l, "/v1/providers/acme/foo/1.0.0/download/linux/amd64")
	if d.Filename != "terraform-provider-foo_1.0.0_linux_amd64.zip" || d.OS != "linux" || d.Arch != "amd64" || len(d.SigningKeys.GPGPublicKeys) != 1 {
		t.Fatalf("download doc: %+v", d)
	}
	if b := mustGet(t, l, localPath(t, d.DownloadURL)); !bytes.Equal(b, amd) {
		t.Fatal("zip mismatch")
	}
	sums := mustGet(t, l, localPath(t, d.SHASumsURL))
	want := sha(arm) + "  terraform-provider-foo_1.0.0_darwin_arm64.zip\n" + sha(amd) + "  terraform-provider-foo_1.0.0_linux_amd64.zip\n"
	if string(sums) != want || d.SHASum != sha(amd) {
		t.Fatalf("sums:\n%s\nwant:\n%s", sums, want)
	}
	sig := mustGet(t, l, localPath(t, d.SHASumsSignatureURL))
	verify(t, d.SigningKeys.GPGPublicKeys[0].ASCIIArmor, sums, sig)
	if len(d.SigningKeys.GPGPublicKeys[0].KeyID) != 16 {
		t.Fatalf("key id %q", d.SigningKeys.GPGPublicKeys[0].KeyID)
	}

	if w := do(l, http.MethodDelete, "/v1/providers/acme/foo/1.0.0/darwin/arm64", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete platform: %d", w.Code)
	}
	sums = mustGet(t, l, localPath(t, d.SHASumsURL))
	if strings.Contains(string(sums), "darwin") {
		t.Fatalf("sums after platform delete: %s", sums)
	}
	if err := l.DeleteVersion(context.Background(), "providers/acme/foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/v1/providers/acme/foo/versions", nil); w.Code != http.StatusNotFound {
		t.Fatalf("versions after delete: %d", w.Code)
	}
}

func TestLocalProvidersPublisherSigned(t *testing.T) {
	l := newLocal(t, "l", "")
	publishProvider(t, l)
	d := getDownload(t, l, "/v1/providers/acme/foo/1.0.0/download/linux/amd64")
	if len(d.SigningKeys.GPGPublicKeys) != 0 {
		t.Fatal("unexpected signing key without SigningKey or upload")
	}
	if w := do(l, http.MethodGet, localPath(t, d.SHASumsSignatureURL), nil); w.Code != http.StatusNotFound {
		t.Fatalf("sig without key: %d", w.Code)
	}

	_, ent := testKey(t)
	sums := mustGet(t, l, localPath(t, d.SHASumsURL))
	var sig bytes.Buffer
	if err := openpgp.DetachSign(&sig, ent, bytes.NewReader(sums), nil); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodPut, "/v1/providers/acme/foo/1.0.0/SHA256SUMS.sig", sig.Bytes()); w.Code != http.StatusCreated {
		t.Fatalf("sig upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/v1/providers/acme/foo/1.0.0/signing-key", []byte("junk")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad key: %d", w.Code)
	}
	if w := do(l, http.MethodPut, "/v1/providers/acme/foo/1.0.0/signing-key", armoredPublic(t, ent)); w.Code != http.StatusCreated {
		t.Fatalf("key upload: %d %s", w.Code, w.Body)
	}
	d = getDownload(t, l, "/v1/providers/acme/foo/1.0.0/download/linux/amd64")
	if len(d.SigningKeys.GPGPublicKeys) != 1 {
		t.Fatalf("keys: %+v", d.SigningKeys)
	}
	verify(t, d.SigningKeys.GPGPublicKeys[0].ASCIIArmor, mustGet(t, l, localPath(t, d.SHASumsURL)), mustGet(t, l, localPath(t, d.SHASumsSignatureURL)))

	if w := do(l, http.MethodPut, "/v1/providers/acme/foo/1.0.0/SHA256SUMS", []byte("custom\n")); w.Code != http.StatusCreated {
		t.Fatalf("sums upload: %d", w.Code)
	}
	if s := string(mustGet(t, l, localPath(t, d.SHASumsURL))); s != "custom\n" {
		t.Fatalf("sums override: %q", s)
	}
}

func TestPromote(t *testing.T) {
	src, dst := newLocal(t, "a", ""), newLocal(t, "b", "")
	publishProvider(t, src)
	do(src, http.MethodPut, "/v1/modules/acme/vpc/aws/1.0.0", tarGz(t, "x"))
	if err := src.PromoteVersion(context.Background(), dst, "providers/acme/foo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := src.PromoteVersion(context.Background(), dst, "modules/acme/vpc/aws", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	getDownload(t, dst, "/v1/providers/acme/foo/1.0.0/download/darwin/arm64")
	mustGet(t, dst, "/v1/modules/acme/vpc/aws/versions")
}

func TestRemote(t *testing.T) {
	up := newLocal(t, "up", "")
	tgz := tarGz(t, "main.tf")
	do(up, http.MethodPut, "/v1/modules/acme/vpc/aws/1.0.0", tgz)
	amd, _ := publishProvider(t, up)
	_, ent := testKey(t)
	sums := mustGet(t, up, "/archive/providers/acme/foo/1.0.0/terraform-provider-foo_1.0.0_SHA256SUMS")
	var sig bytes.Buffer
	_ = openpgp.DetachSign(&sig, ent, bytes.NewReader(sums), nil)
	do(up, http.MethodPut, "/v1/providers/acme/foo/1.0.0/SHA256SUMS.sig", sig.Bytes())
	do(up, http.MethodPut, "/v1/providers/acme/foo/1.0.0/signing-key", armoredPublic(t, ent))

	// The fake upstream advertises its APIs under /api/ and serves a
	// git:: module source for one module.
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/terraform.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"modules.v1":"/api/v1/modules/","providers.v1":"/api/v1/providers/"}`))
	})
	mux.HandleFunc("/api/v1/modules/acme/git/aws/2.0.0/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Terraform-Get", "git::https://example.com/acme/git.git?ref=v2.0.0")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		up.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rr := newRemote(t, srv.URL)

	if vs := string(mustGet(t, rr, "/v1/modules/acme/vpc/aws/versions")); !strings.Contains(vs, `"1.0.0"`) {
		t.Fatalf("remote versions: %s", vs)
	}
	w := do(rr, http.MethodGet, "/v1/modules/acme/vpc/aws/1.0.0/download", nil)
	get := w.Header().Get("X-Terraform-Get")
	if w.Code != http.StatusNoContent || get != "http://kutu.test/archive/modules/acme/vpc/aws/1.0.0.tar.gz" {
		t.Fatalf("remote module download: %d %q", w.Code, get)
	}
	if b := mustGet(t, rr, localPath(t, get)); !bytes.Equal(b, tgz) {
		t.Fatal("remote archive mismatch")
	}
	w = do(rr, http.MethodGet, "/v1/modules/acme/git/aws/2.0.0/download", nil)
	if get := w.Header().Get("X-Terraform-Get"); get != "git::https://example.com/acme/git.git?ref=v2.0.0" {
		t.Fatalf("git passthrough: %q", get)
	}

	if vs := string(mustGet(t, rr, "/v1/providers/acme/foo/versions")); !strings.Contains(vs, `"linux"`) {
		t.Fatalf("provider versions: %s", vs)
	}
	d := getDownload(t, rr, "/v1/providers/acme/foo/1.0.0/download/linux/amd64")
	if !strings.HasPrefix(d.DownloadURL, "http://kutu.test/archive/providers/acme/foo/1.0.0/") || !strings.HasPrefix(d.SHASumsSignatureURL, "http://kutu.test/") {
		t.Fatalf("rewrite: %+v", d)
	}
	if b := mustGet(t, rr, localPath(t, d.DownloadURL)); !bytes.Equal(b, amd) {
		t.Fatal("remote zip mismatch")
	}
	rsums := mustGet(t, rr, localPath(t, d.SHASumsURL))
	rsig := mustGet(t, rr, localPath(t, d.SHASumsSignatureURL))
	verify(t, d.SigningKeys.GPGPublicKeys[0].ASCIIArmor, rsums, rsig)

	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 3 {
		t.Fatalf("remote list: %+v", pkgs)
	}
	srv.Close()
	if b := mustGet(t, rr, localPath(t, d.DownloadURL)); !bytes.Equal(b, amd) {
		t.Fatal("cached zip mismatch")
	}
	getDownload(t, rr, "/v1/providers/acme/foo/1.0.0/download/linux/amd64")
	if w := do(rr, http.MethodPut, "/v1/modules/acme/vpc/aws/3.0.0", tgz); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote write: %d", w.Code)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t, "a", ""), newLocal(t, "b", "")
	do(a, http.MethodPut, "/v1/modules/acme/vpc/aws/1.0.0", tarGz(t, "a"))
	do(b, http.MethodPut, "/v1/modules/acme/vpc/aws/2.0.0", tarGz(t, "b"))
	do(a, http.MethodPut, "/v1/providers/acme/foo/1.0.0/linux/amd64", zipOf(t, "p", "a"))
	do(b, http.MethodPut, "/v1/providers/acme/foo/2.0.0/linux/amd64", zipOf(t, "p", "b"))

	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	if vs := string(mustGet(t, reg, "/v1/modules/acme/vpc/aws/versions")); !strings.Contains(vs, `{"version":"2.0.0"},{"version":"1.0.0"}`) {
		t.Fatalf("virtual module versions: %s", vs)
	}
	if vs := string(mustGet(t, reg, "/v1/providers/acme/foo/versions")); !strings.Contains(vs, `"2.0.0"`) || !strings.Contains(vs, `"1.0.0"`) {
		t.Fatalf("virtual provider versions: %s", vs)
	}
	w := do(reg, http.MethodGet, "/v1/modules/acme/vpc/aws/2.0.0/download", nil)
	if w.Code != http.StatusNoContent || w.Header().Get("X-Terraform-Get") == "" {
		t.Fatalf("virtual download: %d", w.Code)
	}
	getDownload(t, reg, "/v1/providers/acme/foo/1.0.0/download/linux/amd64")
}

func sha(b []byte) string { return pkgbase.SHA256Hex(b) }
