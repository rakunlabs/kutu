package rpm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

// ── minimal RPM writer ──

type hdrEntry struct {
	tag, typ int32
	val      any // string, []string, []int32
}

func buildHeader(entries []hdrEntry) []byte {
	sort.Slice(entries, func(i, j int) bool { return entries[i].tag < entries[j].tag })
	var data bytes.Buffer
	var index bytes.Buffer
	for _, e := range entries {
		var count int
		switch v := e.val.(type) {
		case string:
			off := data.Len()
			data.WriteString(v)
			data.WriteByte(0)
			count = 1
			writeIndex(&index, e.tag, e.typ, off, count)
		case []string:
			off := data.Len()
			for _, s := range v {
				data.WriteString(s)
				data.WriteByte(0)
			}
			writeIndex(&index, e.tag, e.typ, off, len(v))
		case []int32:
			for data.Len()%4 != 0 {
				data.WriteByte(0)
			}
			off := data.Len()
			for _, n := range v {
				_ = binary.Write(&data, binary.BigEndian, n)
			}
			writeIndex(&index, e.tag, e.typ, off, len(v))
		}
	}
	var out bytes.Buffer
	out.Write(headerMagic)
	out.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(&out, binary.BigEndian, int32(len(entries)))
	_ = binary.Write(&out, binary.BigEndian, int32(data.Len()))
	out.Write(index.Bytes())
	out.Write(data.Bytes())
	return out.Bytes()
}

func writeIndex(w *bytes.Buffer, tag, typ int32, off, count int) {
	_ = binary.Write(w, binary.BigEndian, tag)
	_ = binary.Write(w, binary.BigEndian, typ)
	_ = binary.Write(w, binary.BigEndian, int32(off))
	_ = binary.Write(w, binary.BigEndian, int32(count))
}

func buildRPM(t *testing.T, name, version, release, arch string, epoch int32) []byte {
	t.Helper()
	var b bytes.Buffer
	lead := make([]byte, leadSize)
	copy(lead, leadMagic)
	lead[4], lead[5] = 3, 0
	copy(lead[10:], name+"-"+version+"-"+release)
	lead[79] = 5 // signature type: header-style
	b.Write(lead)
	sig := buildHeader([]hdrEntry{{sigTagPayloadSize, typeInt32, []int32{4}}})
	b.Write(sig)
	for (b.Len()-leadSize)%8 != 0 {
		b.WriteByte(0)
	}
	main := []hdrEntry{
		{tagName, typeString, name},
		{tagVersion, typeString, version},
		{tagRelease, typeString, release},
		{tagSummary, typeI18NString, []string{"Test package " + name}},
		{tagDescription, typeI18NString, []string{"A <test> package & more"}},
		{tagBuildTime, typeInt32, []int32{1700000000}},
		{tagSize, typeInt32, []int32{1234}},
		{tagLicense, typeString, "MIT"},
		{tagGroup, typeI18NString, []string{"Unspecified"}},
		{tagURL, typeString, "https://example.com/" + name},
		{tagArch, typeString, arch},
		{tagSourceRPM, typeString, name + "-" + version + "-" + release + ".src.rpm"},
		{tagProvideName, typeStringArray, []string{name, name + "(x86-64)"}},
		{tagProvideFlags, typeInt32, []int32{senseEqual, senseEqual}},
		{tagProvideVersion, typeStringArray, []string{version + "-" + release, version + "-" + release}},
		{tagRequireName, typeStringArray, []string{"rpmlib(CompressedFileNames)", "glibc", "/bin/sh"}},
		{tagRequireFlags, typeInt32, []int32{senseLess | senseEqual, senseGreater | senseEqual, sensePrereq}},
		{tagRequireVersion, typeStringArray, []string{"3.0.4-1", "2.34", ""}},
		{tagDirIndexes, typeInt32, []int32{0, 1, 1}},
		{tagBaseNames, typeStringArray, []string{name, name + ".conf", "README"}},
		{tagDirNames, typeStringArray, []string{"/usr/bin/", "/etc/" + name + "/"}},
		{tagChangelogTime, typeInt32, []int32{1690000000}},
		{tagChangelogName, typeStringArray, []string{"Dev <dev@example.com> - " + version}},
		{tagChangelogText, typeStringArray, []string{"- initial"}},
	}
	if epoch > 0 {
		main = append(main, hdrEntry{tagEpoch, typeInt32, []int32{epoch}})
	}
	b.Write(buildHeader(main))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("cpio"))
	_ = zw.Close()
	b.Write(gz.Bytes())
	return b.Bytes()
}

// ── helpers ──

func testKey(t *testing.T) (string, openpgp.EntityList) {
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
	return buf.String(), openpgp.EntityList{e}
}

func newLocal(t *testing.T, name, key string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "rpm", AllowPush: true, SigningKey: key}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/yum")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type repomdTest struct {
	Data []struct {
		Type         string `xml:"type,attr"`
		Checksum     string `xml:"checksum"`
		OpenChecksum string `xml:"open-checksum"`
		Location     struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Size     int64 `xml:"size"`
		OpenSize int64 `xml:"open-size"`
	} `xml:"data"`
}

// fetchRepo downloads repomd + documents, verifying checksums, and
// returns the decompressed documents by type.
func fetchRepo(t *testing.T, h http.Handler) (map[string]string, []byte) {
	t.Helper()
	w := do(h, http.MethodGet, "/repodata/repomd.xml", nil)
	if w.Code != 200 {
		t.Fatalf("repomd: %d %s", w.Code, w.Body)
	}
	repomd := w.Body.Bytes()
	var md repomdTest
	if err := xml.Unmarshal(repomd, &md); err != nil {
		t.Fatal(err)
	}
	if len(md.Data) != 3 {
		t.Fatalf("repomd data: %+v", md)
	}
	docs := map[string]string{}
	for _, d := range md.Data {
		if !strings.HasPrefix(d.Location.Href, "repodata/"+d.Checksum+"-") {
			t.Fatalf("href %q lacks checksum prefix", d.Location.Href)
		}
		gw := do(h, http.MethodGet, "/"+d.Location.Href, nil)
		if gw.Code != 200 {
			t.Fatalf("get %s: %d", d.Location.Href, gw.Code)
		}
		gz := gw.Body.Bytes()
		if sha256Hex(gz) != d.Checksum || int64(len(gz)) != d.Size {
			t.Fatalf("%s checksum/size mismatch", d.Type)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		open, _ := io.ReadAll(zr)
		if sha256Hex(open) != d.OpenChecksum || int64(len(open)) != d.OpenSize {
			t.Fatalf("%s open checksum/size mismatch", d.Type)
		}
		docs[d.Type] = string(open)
	}
	return docs, repomd
}

// ── tests ──

func TestParseRPM(t *testing.T) {
	b := buildRPM(t, "hello", "1.2", "3.fc40", "x86_64", 2)
	p, err := ParseRPM(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "hello" || p.Version != "1.2" || p.Release != "3.fc40" || p.Arch != "x86_64" || p.Epoch != "2" {
		t.Fatalf("nevra: %+v", p)
	}
	if p.License != "MIT" || p.BuildTime != 1700000000 || p.InstalledSize != 1234 || p.ArchiveSize != 4 {
		t.Fatalf("meta: %+v", p)
	}
	if p.HeaderStart <= leadSize || p.HeaderEnd <= p.HeaderStart || p.HeaderStart%8 != 0 {
		t.Fatalf("header range: %d-%d", p.HeaderStart, p.HeaderEnd)
	}
	if len(p.Requires) != 2 || p.Requires[0].Name != "glibc" || p.Requires[0].Flags != "GE" || !p.Requires[1].Pre {
		t.Fatalf("requires: %+v", p.Requires)
	}
	if len(p.Files) != 3 || p.Files[0].Path != "/usr/bin/hello" || p.Files[1].Path != "/etc/hello/hello.conf" {
		t.Fatalf("files: %+v", p.Files)
	}
	if p.EVR() != "2:1.2-3.fc40" || p.FileName() != "hello-1.2-3.fc40.x86_64.rpm" {
		t.Fatalf("evr/file: %s %s", p.EVR(), p.FileName())
	}
	if _, err := ParseRPM([]byte("nope")); err == nil {
		t.Fatal("expected error for garbage")
	}
}

func TestLocalPublishSignDelete(t *testing.T) {
	key, ring := testKey(t)
	l := newLocal(t, "yum", key)

	rpm1 := buildRPM(t, "hello", "1.0", "1", "x86_64", 0)
	if w := do(l, http.MethodPut, "/upload", rpm1); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload", buildRPM(t, "hello", "1.1", "1", "x86_64", 0)); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/upload", []byte("not an rpm")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad upload: %d", w.Code)
	}

	docs, repomd := fetchRepo(t, l)
	primary := docs["primary"]
	for _, want := range []string{
		`packages="2"`,
		`<name>hello</name>`,
		`<version epoch="0" ver="1.0" rel="1"></version>`,
		`<checksum type="sha256" pkgid="YES">` + sha256Hex(rpm1) + `</checksum>`,
		`<location href="Packages/h/hello-1.0-1.x86_64.rpm"></location>`,
		`<rpm:license>MIT</rpm:license>`,
		`<rpm:header-range start=`,
		`<rpm:entry name="glibc" flags="GE" epoch="0" ver="2.34"></rpm:entry>`,
		`<file>/usr/bin/hello</file>`,
		`xmlns:rpm="http://linux.duke.edu/metadata/rpm"`,
	} {
		if !strings.Contains(primary, want) {
			t.Fatalf("primary missing %q:\n%s", want, primary)
		}
	}
	if strings.Contains(primary, "rpmlib(") {
		t.Fatalf("primary has rpmlib deps:\n%s", primary)
	}
	if !strings.Contains(docs["filelists"], "<file>/etc/hello/README</file>") {
		t.Fatalf("filelists: %s", docs["filelists"])
	}
	if !strings.Contains(docs["other"], `<changelog author="Dev`) {
		t.Fatalf("other: %s", docs["other"])
	}
	if err := xml.Unmarshal([]byte(primary), new(struct{})); err != nil {
		t.Fatalf("primary not well-formed: %v", err)
	}

	sig := do(l, http.MethodGet, "/repodata/repomd.xml.asc", nil)
	if sig.Code != 200 {
		t.Fatalf("asc: %d", sig.Code)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(ring, bytes.NewReader(repomd), bytes.NewReader(sig.Body.Bytes()), nil); err != nil {
		t.Fatalf("signature: %v", err)
	}
	for _, p := range []string{"/repodata/repomd.xml.key", "/RPM-GPG-KEY"} {
		if w := do(l, http.MethodGet, p, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "PUBLIC KEY") {
			t.Fatalf("key %s: %d", p, w.Code)
		}
	}
	rf := do(l, http.MethodGet, "/config.repo", nil).Body.String()
	if !strings.Contains(rf, "baseurl=http://example.com/registries/default/yum") || !strings.Contains(rf, "repo_gpgcheck=1") ||
		!strings.Contains(rf, "gpgkey=http://example.com/registries/default/yum/repodata/repomd.xml.key") {
		t.Fatalf("repo file:\n%s", rf)
	}

	if w := do(l, http.MethodGet, "/Packages/h/hello-1.0-1.x86_64.rpm", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), rpm1) {
		t.Fatalf("download: %d", w.Code)
	}

	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "hello" || strings.Join(pkgs[0].Versions, ",") != "1.0-1,1.1-1" {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(context.Background(), "hello")
	if err != nil || d.Generic.LatestVersion != "1.1-1" || d.Generic.License != "MIT" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/Packages/h/hello-1.0-1.x86_64.rpm", nil))
	if !ok || ref.Name != "hello" || ref.Version != "1.0-1" {
		t.Fatalf("classify: %+v %v", ref, ok)
	}
	info, err := l.ArtifactInfo(context.Background(), ref)
	if err != nil || info.License != "MIT" || info.PublishedAt.Unix() != 1700000000 {
		t.Fatalf("info: %+v %v", info, err)
	}

	oldFiles, _ := l.store.ListFiles(repodataDir)
	if err := l.DeleteVersion(context.Background(), "hello", "1.0-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(context.Background(), "hello", "1.0-1"); err != registry.ErrPackageNotFound {
		t.Fatalf("second delete: %v", err)
	}
	docs, _ = fetchRepo(t, l)
	if strings.Contains(docs["primary"], `ver="1.0"`) || !strings.Contains(docs["primary"], `packages="1"`) {
		t.Fatalf("primary after delete:\n%s", docs["primary"])
	}
	newFiles, _ := l.store.ListFiles(repodataDir)
	if len(newFiles) != len(oldFiles) || len(newFiles) != 6 {
		t.Fatalf("stale repodata files: %d vs %d", len(newFiles), len(oldFiles))
	}
	if w := do(l, http.MethodDelete, "/Packages/h/hello-1.1-1.x86_64.rpm", nil); w.Code != http.StatusNoContent {
		t.Fatalf("http delete: %d", w.Code)
	}
	docs, _ = fetchRepo(t, l)
	if !strings.Contains(docs["primary"], `packages="0"`) {
		t.Fatalf("primary after http delete:\n%s", docs["primary"])
	}
}

func TestPromote(t *testing.T) {
	src := newLocal(t, "a", "")
	dst := newLocal(t, "b", "")
	do(src, http.MethodPut, "/upload", buildRPM(t, "tool", "2.0", "1", "noarch", 1))
	if err := src.PromoteVersion(context.Background(), dst, "tool", "1:2.0-1"); err != nil {
		t.Fatal(err)
	}
	docs, _ := fetchRepo(t, dst)
	if !strings.Contains(docs["primary"], `<version epoch="1" ver="2.0" rel="1">`) {
		t.Fatalf("promoted primary:\n%s", docs["primary"])
	}
	if w := do(dst, http.MethodGet, "/repodata/repomd.xml.asc", nil); w.Code != 404 {
		t.Fatalf("unsigned repo asc: %d", w.Code)
	}
}

func TestRemote(t *testing.T) {
	up := newLocal(t, "up", "")
	rpm1 := buildRPM(t, "hello", "1.0", "1", "x86_64", 0)
	do(up, http.MethodPut, "/upload", rpm1)
	srv := httptest.NewServer(up)
	defer srv.Close()

	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	docs, _ := fetchRepo(t, rr)
	if !strings.Contains(docs["primary"], "hello-1.0-1.x86_64.rpm") {
		t.Fatalf("remote primary: %s", docs["primary"])
	}
	if w := do(rr, http.MethodGet, "/Packages/h/hello-1.0-1.x86_64.rpm", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), rpm1) {
		t.Fatalf("remote rpm: %d", w.Code)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Versions[0] != "1.0-1" {
		t.Fatalf("remote list: %+v", pkgs)
	}
	if info, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "hello", Version: "1.0-1"}); err != nil || info.License != "MIT" {
		t.Fatalf("remote info: %+v %v", info, err)
	}
	if err := rr.Prefetch(context.Background(), "hello", ""); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/Packages/h/hello-1.0-1.x86_64.rpm", nil); w.Code != 200 {
		t.Fatalf("cached rpm: %d", w.Code)
	}
	if w := do(rr, http.MethodPut, "/upload", rpm1); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote write: %d", w.Code)
	}
	st, _ := rr.PurgeCache(context.Background(), registry.PurgeOptions{})
	if st.PurgedFiles != 1 || st.Skipped == 0 {
		t.Fatalf("purge: %+v", st)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVirtual(t *testing.T) {
	a := newLocal(t, "a", "")
	b := newLocal(t, "b", "")
	do(a, http.MethodPut, "/upload", buildRPM(t, "alpha", "1.0", "1", "x86_64", 0))
	do(a, http.MethodPut, "/upload", buildRPM(t, "shared", "1.0", "1", "noarch", 0))
	do(b, http.MethodPut, "/upload", buildRPM(t, "beta", "2.0", "1", "x86_64", 0))
	do(b, http.MethodPut, "/upload", buildRPM(t, "shared", "1.0", "1", "noarch", 0))

	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	docs, _ := fetchRepo(t, reg)
	for kind, doc := range docs {
		if !strings.Contains(doc, `packages="3"`) {
			t.Fatalf("%s: %s", kind, doc)
		}
		if n := len(regexp.MustCompile(`<package[ >]`).FindAllString(doc, -1)); n != 3 {
			t.Fatalf("%s has %d packages", kind, n)
		}
	}
	if !strings.Contains(docs["primary"], "<name>alpha</name>") || !strings.Contains(docs["primary"], "<name>beta</name>") {
		t.Fatalf("merged primary: %s", docs["primary"])
	}
	if w := do(reg, http.MethodGet, "/Packages/b/beta-2.0-1.x86_64.rpm", nil); w.Code != 200 {
		t.Fatalf("virtual rpm: %d", w.Code)
	}
	if w := do(reg, http.MethodGet, "/repodata/repomd.xml.asc", nil); w.Code != 404 {
		t.Fatalf("virtual asc: %d", w.Code)
	}
	pkgs, _ := reg.(registry.PackageLister).ListPackages(context.Background())
	if len(pkgs) != 3 {
		t.Fatalf("virtual list: %+v", pkgs)
	}
}
