// Package cran implements a CRAN-like R package repository:
//
//	GET    /src/contrib/PACKAGES[.gz]                              source index (latest version per package)
//	GET    /src/contrib/{pkg}_{ver}.tar.gz                         latest source package
//	GET    /src/contrib/Archive/{pkg}/{pkg}_{ver}.tar.gz           older source packages
//	GET    /bin/{platform...}/contrib/{rver}/PACKAGES[.gz]         binary index
//	GET    /bin/{platform...}/contrib/{rver}/{pkg}_{ver}.{zip|tgz} binary package
//	PUT    /upload                                                 source upload (local, allow_push)
//	PUT    /upload/bin/{platform...}/{rver}                        binary upload (windows → .zip, macosx → .tgz)
//	PUT    /src/contrib/{file}, /bin/{platform...}/contrib/{rver}/{file}   upload to the wire path
//	DELETE /src/contrib/{file} (or any stored wire path)           delete one file
//
// Metadata comes from {pkg}/DESCRIPTION inside the archive. Following
// CRAN semantics the newest source version lives in src/contrib and
// older ones move to src/contrib/Archive/{pkg}/; PACKAGES lists only
// the newest version of each package. PACKAGES.rds is not produced
// (404; R falls back to PACKAGES.gz). Remote repos proxy a CRAN mirror
// such as https://cloud.r-project.org: PACKAGES* honour MutableTTL,
// package files are cached forever (and a source tarball that upstream
// has moved to Archive is retried there). Virtual repos merge PACKAGES
// across members (highest version wins per package).
//
// Client configuration:
//
//	options(repos = c(kutu = "https://kutu.example.com/registries/{ns}/{repo}"))
//	# ~/.netrc: machine kutu.example.com login x password <token>
//	curl -u x:<token> -T pkg_1.0.tar.gz https://kutu.example.com/registries/{ns}/{repo}/upload
package cran

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeCRAN

const (
	filesDir   = "files"
	recordsDir = "records"
	indexDir   = "index"
	srcDir     = "src/contrib"
)

var (
	_ registry.PackageLister        = (*Local)(nil)
	_ registry.PackageDetailer      = (*Local)(nil)
	_ registry.VersionDeleter       = (*Local)(nil)
	_ registry.ArtifactClassifier   = (*Local)(nil)
	_ registry.ArtifactInfoProvider = (*Local)(nil)
	_ registry.VersionPromoter      = (*Local)(nil)
	_ registry.StatsProvider        = (*Local)(nil)
)

var (
	pkgNameRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]*$`)
	versionRe  = regexp.MustCompile(`^[0-9]+([.-][0-9]+)*$`)
	rVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	segmentRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// stanzaFields are the DESCRIPTION fields copied into PACKAGES, in order.
var stanzaFields = []string{
	"Package", "Version", "Priority", "Depends", "Imports", "LinkingTo", "Suggests", "Enhances",
	"License", "License_is_FOSS", "License_restricts_use", "OS_type", "Archs", "NeedsCompilation",
}

// CompareRVersions compares R package versions ("1.2-3", "0.10.1"):
// every "."/"-" separated component is compared numerically.
func CompareRVersions(a, b string) int {
	split := func(v string) []string {
		return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' })
	}
	as, bs := split(a), split(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int64 = -1, -1
		if i < len(as) {
			x, _ = strconv.ParseInt(as[i], 10, 64)
		}
		if i < len(bs) {
			y, _ = strconv.ParseInt(bs[i], 10, 64)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(a, b)
}

func sortRVersions(vs []string) {
	sort.SliceStable(vs, func(i, j int) bool { return CompareRVersions(vs[i], vs[j]) < 0 })
}

// ── DCF ──

// Stanza is one DCF record with field order preserved.
type Stanza struct {
	Keys   []string
	Values map[string]string
}

// Get returns field k.
func (s *Stanza) Get(k string) string { return s.Values[k] }

// Set sets field k (appending to the key order when new).
func (s *Stanza) Set(k, v string) {
	if s.Values == nil {
		s.Values = map[string]string{}
	}
	if _, ok := s.Values[k]; !ok {
		s.Keys = append(s.Keys, k)
	}
	s.Values[k] = v
}

// ParseDCF parses Debian-control-format records (DESCRIPTION / PACKAGES).
// Continuation lines are folded into a single space-joined value.
func ParseDCF(r io.Reader) ([]*Stanza, error) {
	var out []*Stanza
	var cur *Stanza
	var last string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			cur, last = nil, ""
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if cur != nil && last != "" {
				cur.Values[last] = strings.TrimSpace(cur.Values[last] + " " + strings.TrimSpace(line))
			}
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		if cur == nil {
			cur = &Stanza{Values: map[string]string{}}
			out = append(out, cur)
		}
		last = line[:i]
		cur.Set(last, strings.TrimSpace(line[i+1:]))
	}
	return out, sc.Err()
}

// WriteDCF renders stanzas separated by blank lines.
func WriteDCF(stanzas []*Stanza) []byte {
	var buf bytes.Buffer
	for i, s := range stanzas {
		if i > 0 {
			buf.WriteByte('\n')
		}
		for _, k := range s.Keys {
			fmt.Fprintf(&buf, "%s: %s\n", k, s.Values[k])
		}
	}
	return buf.Bytes()
}

// ── Archive parsing ──

// ReadDescription extracts {pkg}/DESCRIPTION from a .tar.gz/.tgz or .zip package.
func ReadDescription(body []byte) (*Stanza, error) {
	return readDescription(bytes.NewReader(body), int64(len(body)))
}

func readDescription(ra io.ReaderAt, size int64) (*Stanza, error) {
	magic := make([]byte, 4)
	n, _ := ra.ReadAt(magic, 0)
	magic = magic[:n]
	var raw []byte
	var err error
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		raw, err = descFromTarGz(io.NewSectionReader(ra, 0, size))
	case bytes.HasPrefix(magic, []byte("PK\x03\x04")):
		raw, err = descFromZip(ra, size)
	default:
		return nil, errors.New("body is not a .tar.gz or .zip R package")
	}
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("package has no {pkg}/DESCRIPTION")
	}
	st, err := ParseDCF(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if len(st) == 0 {
		return nil, errors.New("empty DESCRIPTION")
	}
	d := st[0]
	if !pkgNameRe.MatchString(d.Get("Package")) || !versionRe.MatchString(d.Get("Version")) {
		return nil, fmt.Errorf("DESCRIPTION has invalid Package/Version %q/%q", d.Get("Package"), d.Get("Version"))
	}
	return d, nil
}

func descFromTarGz(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading package tar: %w", err)
		}
		if isDescPath(hdr.Name) {
			return io.ReadAll(io.LimitReader(tr, 4<<20))
		}
	}
}

func descFromZip(ra io.ReaderAt, size int64) ([]byte, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if !isDescPath(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 4<<20))
	}
	return nil, nil
}

func isDescPath(name string) bool {
	parts := strings.Split(strings.TrimPrefix(name, "./"), "/")
	return len(parts) == 2 && parts[1] == "DESCRIPTION"
}

// SplitFilename splits "{pkg}_{ver}.{tar.gz|zip|tgz}".
func SplitFilename(fn string) (name, version, ext string, ok bool) {
	for _, e := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(fn, e) {
			ext = e
			break
		}
	}
	if ext == "" {
		return "", "", "", false
	}
	stem := strings.TrimSuffix(fn, ext)
	i := strings.IndexByte(stem, '_')
	if i <= 0 {
		return "", "", "", false
	}
	name, version = stem[:i], stem[i+1:]
	if !pkgNameRe.MatchString(name) || !versionRe.MatchString(version) {
		return "", "", "", false
	}
	return name, version, ext, true
}

// ── Wire paths ──

// wirePath is a parsed repository path.
type wirePath struct {
	dir   string // "src/contrib" or "bin/{platform...}/contrib/{rver}"
	file  string // basename ("" for directories)
	index bool   // PACKAGES*
}

// parseWire parses a request path into its repository dir and file.
func parseWire(p string) (wirePath, bool) {
	p = strings.Trim(p, "/")
	parts := strings.Split(p, "/")
	for _, s := range parts {
		if !segmentRe.MatchString(s) {
			return wirePath{}, false
		}
	}
	var dir string
	var rest []string
	switch {
	case len(parts) >= 3 && parts[0] == "src" && parts[1] == "contrib":
		dir, rest = srcDir, parts[2:]
	case len(parts) >= 5 && parts[0] == "bin":
		i := indexOf(parts, "contrib")
		if i < 2 || i+1 >= len(parts) || !rVersionRe.MatchString(parts[i+1]) {
			return wirePath{}, false
		}
		dir, rest = strings.Join(parts[:i+2], "/"), parts[i+2:]
	default:
		return wirePath{}, false
	}
	switch {
	case len(rest) == 1:
		if strings.HasPrefix(rest[0], "PACKAGES") {
			return wirePath{dir: dir, file: rest[0], index: true}, true
		}
		if _, _, _, ok := SplitFilename(rest[0]); ok {
			return wirePath{dir: dir, file: rest[0]}, true
		}
	case len(rest) == 3 && dir == srcDir && rest[0] == "Archive":
		if name, _, ext, ok := SplitFilename(rest[2]); ok && name == rest[1] && ext == ".tar.gz" {
			return wirePath{dir: dir, file: rest[2]}, true
		}
	}
	return wirePath{}, false
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

func archivePath(name, file string) string { return path.Join(srcDir, "Archive", name, file) }

// ── Records ──

// Record is the stored metadata of one package file.
type Record struct {
	Dir      string            `json:"dir"`
	File     string            `json:"file"`
	Path     string            `json:"path"` // wire path of the stored file
	Fields   map[string]string `json:"fields"`
	Keys     []string          `json:"keys"`
	MD5      string            `json:"md5"`
	SHA256   string            `json:"sha256,omitempty"`
	Size     int64             `json:"size"`
	Uploaded time.Time         `json:"uploaded"`
}

func (r *Record) Name() string    { return r.Fields["Package"] }
func (r *Record) Version() string { return r.Fields["Version"] }

// Time returns Date/Publication, else the Built date, else upload time.
func (r *Record) Time() time.Time {
	for _, v := range []string{r.Fields["Date/Publication"], r.Fields["Packaged"]} {
		if v == "" {
			continue
		}
		v = strings.TrimSpace(strings.SplitN(v, ";", 2)[0])
		for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 UTC", "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC()
			}
		}
	}
	return r.Uploaded
}

func newRecord(dir, file, wire string, d *Stanza, body []byte) *Record {
	sum := md5.Sum(body)
	return &Record{
		Dir: dir, File: file, Path: wire, Fields: d.Values, Keys: d.Keys,
		MD5: hex.EncodeToString(sum[:]), SHA256: pkgbase.SHA256Hex(body),
		Size: int64(len(body)), Uploaded: time.Now().UTC(),
	}
}

// stanza renders the PACKAGES entry of rec.
func (r *Record) stanza() *Stanza {
	s := &Stanza{}
	for _, k := range stanzaFields {
		if v := r.Fields[k]; v != "" {
			s.Set(k, v)
		}
	}
	s.Set("MD5sum", r.MD5)
	if b := r.Fields["Built"]; b != "" && r.Dir != srcDir {
		s.Set("Built", b)
	}
	return s
}

// ── Store ──

// Store wraps pkgbase.Store with the cran layout: files/{wire path},
// records/{dir}/{file}.json, index/{dir}/PACKAGES[.gz].
type Store struct{ *pkgbase.Store }

func (s *Store) fileRel(wire string) string        { return path.Join(filesDir, wire) }
func (s *Store) recordRel(dir, file string) string { return path.Join(recordsDir, dir, file+".json") }

// PutRecord stores rec.
func (s *Store) PutRecord(rec *Record) error {
	b, err := pkgbase.MarshalJSON(rec)
	if err != nil {
		return err
	}
	return s.Write(s.recordRel(rec.Dir, rec.File), b)
}

// Record loads the record of dir/file.
func (s *Store) Record(dir, file string) (*Record, error) {
	b, err := s.Read(s.recordRel(dir, file))
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Records returns the records of dir ("" = every dir).
func (s *Store) Records(dir string) ([]*Record, error) {
	var out []*Record
	add := func(rel string) {
		if !strings.HasSuffix(rel, ".json") {
			return
		}
		b, err := s.Read(rel)
		if err != nil {
			return
		}
		var rec Record
		if json.Unmarshal(b, &rec) == nil && rec.Fields != nil {
			out = append(out, &rec)
		}
	}
	if dir != "" {
		files, err := s.ListFiles(path.Join(recordsDir, dir))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			add(path.Join(recordsDir, dir, f.Name))
		}
		return out, nil
	}
	err := s.Walk(recordsDir, func(rel string, _ rawfs.DirEntry) error {
		add(rel)
		return nil
	})
	return out, err
}

// latestPerPackage keeps the highest version of each package.
func latestPerPackage(recs []*Record) map[string]*Record {
	out := map[string]*Record{}
	for _, rec := range recs {
		cur, ok := out[rec.Name()]
		if !ok || CompareRVersions(rec.Version(), cur.Version()) > 0 {
			out[rec.Name()] = rec
		}
	}
	return out
}

// BuildPackages renders the PACKAGES file of dir.
func (s *Store) BuildPackages(dir string) ([]byte, error) {
	recs, err := s.Records(dir)
	if err != nil {
		return nil, err
	}
	latest := latestPerPackage(recs)
	names := make([]string, 0, len(latest))
	for n := range latest {
		names = append(names, n)
	}
	sort.Strings(names)
	stanzas := make([]*Stanza, 0, len(names))
	for _, n := range names {
		stanzas = append(stanzas, latest[n].stanza())
	}
	return WriteDCF(stanzas), nil
}

// Reindex regenerates PACKAGES and PACKAGES.gz of dir.
func (s *Store) Reindex(dir string) error {
	body, err := s.BuildPackages(dir)
	if err != nil {
		return err
	}
	if err := s.Write(path.Join(indexDir, dir, "PACKAGES"), body); err != nil {
		return err
	}
	return s.Write(path.Join(indexDir, dir, "PACKAGES.gz"), gzipBytes(body))
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// ListPackages groups records by package name.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	recs, err := s.Records("")
	if err != nil {
		return nil, err
	}
	set := map[string]map[string]struct{}{}
	for _, rec := range recs {
		if set[rec.Name()] == nil {
			set[rec.Name()] = map[string]struct{}{}
		}
		set[rec.Name()][rec.Version()] = struct{}{}
	}
	out := make([]registry.PackageSummary, 0, len(set))
	for name, vs := range set {
		versions := make([]string, 0, len(vs))
		for v := range vs {
			versions = append(versions, v)
		}
		sortRVersions(versions)
		out = append(out, registry.PackageSummary{Name: name, Versions: versions})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) versionRecords(name, version string) ([]*Record, error) {
	recs, err := s.Records("")
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, rec := range recs {
		if rec.Name() == name && (version == "" || rec.Version() == version) {
			out = append(out, rec)
		}
	}
	return out, nil
}

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	recs, err := s.versionRecords(name, "")
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	byVer := map[string][]*Record{}
	var versions []string
	for _, rec := range recs {
		if _, ok := byVer[rec.Version()]; !ok {
			versions = append(versions, rec.Version())
		}
		byVer[rec.Version()] = append(byVer[rec.Version()], rec)
	}
	sortRVersions(versions)
	latest := versions[len(versions)-1]
	d := &registry.GenericPackageDetail{LatestVersion: latest}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		row := registry.GenericVersionDetail{Version: v}
		var newest time.Time
		for _, rec := range byVer[v] {
			row.Files = append(row.Files, registry.GenericFile{Name: rec.Path, Size: rec.Size, SHA256: rec.SHA256})
			row.Size += rec.Size
			if t := rec.Time(); t.After(newest) {
				newest = t
			}
		}
		sort.Slice(row.Files, func(a, b int) bool { return row.Files[a].Name < row.Files[b].Name })
		if !newest.IsZero() {
			row.PublishedAt = newest.Format(time.RFC3339)
		}
		meta := map[string]string{}
		for _, k := range []string{"Depends", "Imports", "LinkingTo", "Suggests", "NeedsCompilation"} {
			if val := byVer[v][0].Fields[k]; val != "" {
				meta[k] = val
			}
		}
		if len(meta) > 0 {
			row.Metadata = meta
		}
		d.Versions = append(d.Versions, row)
	}
	top := byVer[latest][0]
	d.License = top.Fields["License"]
	d.Description = top.Fields["Title"]
	if urls := strings.FieldsFunc(top.Fields["URL"], func(r rune) bool { return r == ',' || r == ' ' }); len(urls) > 0 {
		d.Homepage = urls[0]
	}
	meta := map[string]string{}
	for _, k := range []string{"Description", "Maintainer", "Author", "BugReports"} {
		if v := top.Fields[k]; v != "" {
			meta[strings.ToLower(k)] = v
		}
	}
	if len(meta) > 0 {
		d.Metadata = meta
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if ref.Version == "" {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	recs, err := s.versionRecords(ref.Name, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	if len(recs) == 0 {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	var m registry.ArtifactMeta
	for _, rec := range recs {
		if m.License == "" {
			m.License = rec.Fields["License"]
		}
		if t := rec.Time(); m.PublishedAt.IsZero() || t.Before(m.PublishedAt) {
			m.PublishedAt = t
		}
	}
	return m, nil
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	wp, ok := parseWire(r.URL.Path)
	if !ok || wp.index {
		return registry.ArtifactRef{}, false
	}
	name, version, _, _ := SplitFilename(wp.file)
	return registry.ArtifactRef{Name: name, Version: version}, true
}

// ── Local ──

type Local struct {
	*pkgbase.Repo
	store *Store
	mu    sync.Mutex
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		return &Local{Repo: base, store: &Store{base.Store}}, nil
	}
}

func (l *Local) Close() error  { return nil }
func (l *Local) Store() *Store { return l.store }

func (l *Local) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return l.store.ListPackages(ctx)
}

func (l *Local) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, l, l.store.Store), nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return detail(l.store, name)
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref)
}

// DeleteVersion removes every file (source and binaries) of name@version.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if version == "" {
		return registry.ErrPackageNotFound
	}
	recs, err := l.store.versionRecords(name, version)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return registry.ErrPackageNotFound
	}
	if err := l.deleteRecords(recs); err != nil {
		return err
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

func (l *Local) deleteRecords(recs []*Record) error {
	dirs := map[string]bool{}
	for _, rec := range recs {
		if err := l.store.Delete(l.store.fileRel(rec.Path)); err != nil {
			return err
		}
		if err := l.store.Delete(l.store.recordRel(rec.Dir, rec.File)); err != nil {
			return err
		}
		dirs[rec.Dir] = true
		if rec.Dir == srcDir {
			if err := l.arrangeSource(rec.Name()); err != nil {
				return err
			}
		}
	}
	for d := range dirs {
		if err := l.store.Reindex(d); err != nil {
			return err
		}
	}
	return nil
}

// arrangeSource keeps the newest source version of name in
// src/contrib and moves every older one to src/contrib/Archive/{name}/.
func (l *Local) arrangeSource(name string) error {
	recs, err := l.store.Records(srcDir)
	if err != nil {
		return err
	}
	var mine []*Record
	for _, rec := range recs {
		if rec.Name() == name {
			mine = append(mine, rec)
		}
	}
	latest := latestPerPackage(mine)[name]
	for _, rec := range mine {
		want := archivePath(name, rec.File)
		if rec == latest {
			want = path.Join(srcDir, rec.File)
		}
		if rec.Path == want {
			continue
		}
		body, err := l.store.Read(l.store.fileRel(rec.Path))
		if err != nil {
			return err
		}
		if err := l.store.Write(l.store.fileRel(want), body); err != nil {
			return err
		}
		if err := l.store.Delete(l.store.fileRel(rec.Path)); err != nil {
			return err
		}
		rec.Path = want
		if err := l.store.PutRecord(rec); err != nil {
			return err
		}
	}
	return nil
}

// PromoteVersion copies every file of name@version into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("cran: promote target %T is not a cran local registry", dst)
	}
	if version == "" {
		return registry.ErrPackageNotFound
	}
	recs, err := l.store.versionRecords(name, version)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, rec := range recs {
		body, err := l.store.Read(l.store.fileRel(rec.Path))
		if err != nil {
			return fmt.Errorf("cran: read %s: %w", rec.Path, err)
		}
		if _, _, err := d.publish(rec.Dir, body); err != nil {
			return err
		}
	}
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		l.serveRead(w, r)
	case http.MethodPut, http.MethodPost:
		l.upload(w, r)
	case http.MethodDelete:
		l.remove(w, r)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) serveRead(w http.ResponseWriter, r *http.Request) {
	wp, ok := parseWire(r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	if wp.index {
		if wp.file != "PACKAGES" && wp.file != "PACKAGES.gz" {
			pkgbase.NotFound(w)
			return
		}
		ct := "text/plain; charset=utf-8"
		if wp.file == "PACKAGES.gz" {
			ct = "application/x-gzip"
		}
		if pkgbase.ServeStored(w, r, l.store.Store, path.Join(indexDir, wp.dir, wp.file), ct) {
			return
		}
		body, err := l.store.BuildPackages(wp.dir)
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if wp.file == "PACKAGES.gz" {
			body = gzipBytes(body)
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, ct, body)
		return
	}
	if !serveFile(w, r, l.store, wp) {
		pkgbase.NotFound(w)
	}
}

// serveFile serves a package file by its wire path, falling back to
// wherever the record says it lives (e.g. moved to Archive).
func serveFile(w http.ResponseWriter, r *http.Request, s *Store, wp wirePath) bool {
	if pkgbase.ServeStored(w, r, s.Store, s.fileRel(strings.Trim(r.URL.Path, "/")), "application/octet-stream") {
		return true
	}
	rec, err := s.Record(wp.dir, wp.file)
	if err != nil || rec.Path == "" {
		return false
	}
	return pkgbase.ServeStored(w, r, s.Store, s.fileRel(rec.Path), "application/octet-stream")
}

// uploadDir maps an upload path to the target repository dir.
func uploadDir(p string) (string, bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 0 {
		return "", false
	}
	if parts[0] == "upload" {
		if len(parts) == 1 {
			return srcDir, true
		}
		if parts[1] == "src" && len(parts) == 2 {
			return srcDir, true
		}
		if parts[1] != "bin" || len(parts) < 4 {
			return "", false
		}
		plat, rver := parts[2:len(parts)-1], parts[len(parts)-1]
		if !rVersionRe.MatchString(rver) {
			return "", false
		}
		for _, s := range plat {
			if !segmentRe.MatchString(s) || s == "contrib" {
				return "", false
			}
		}
		return path.Join("bin", strings.Join(plat, "/"), "contrib", rver), true
	}
	wp, ok := parseWire(p)
	if !ok || wp.index {
		return "", false
	}
	return wp.dir, true
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	dir, ok := uploadDir(r.URL.Path)
	if !ok {
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /upload or PUT /upload/bin/{platform}/{rversion}")
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	rec, code, err := l.publish(dir, body)
	if err != nil {
		pkgbase.Error(w, code, err.Error())
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"package": rec.Name(), "version": rec.Version(), "path": rec.Path, "size": rec.Size, "md5": rec.MD5, "sha256": rec.SHA256,
	})
}

// publish stores one package file into dir and regenerates its index.
func (l *Local) publish(dir string, body []byte) (*Record, int, error) {
	desc, err := ReadDescription(body)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	isZip := bytes.HasPrefix(body, []byte("PK\x03\x04"))
	name, version := desc.Get("Package"), desc.Get("Version")
	var ext string
	switch {
	case dir == srcDir && isZip:
		return nil, http.StatusBadRequest, errors.New("source packages must be .tar.gz")
	case dir == srcDir:
		ext = ".tar.gz"
	case isZip:
		ext = ".zip"
	default:
		ext = ".tgz"
	}
	if dir != srcDir && desc.Get("Built") == "" {
		return nil, http.StatusBadRequest, errors.New("binary package DESCRIPTION has no Built field")
	}
	file := name + "_" + version + ext
	wire := path.Join(dir, file)

	l.mu.Lock()
	defer l.mu.Unlock()
	exists := l.store.Exists(l.store.recordRel(dir, file))
	if code, err := l.Guard.Check(l.store.Store, exists, int64(len(body))); err != nil {
		return nil, code, err
	}
	if exists {
		if b, err := l.store.Read(l.store.recordRel(dir, file)); err == nil {
			var old Record
			if json.Unmarshal(b, &old) == nil && old.Path != "" {
				wire = old.Path
			}
		}
	}
	rec := newRecord(dir, file, wire, desc, body)
	if err := l.store.Write(l.store.fileRel(wire), body); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := l.store.PutRecord(rec); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if dir == srcDir {
		if err := l.arrangeSource(name); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}
	if err := l.store.Reindex(dir); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	l.EmitPublished(wire, rec.Size)
	if b, err := l.store.Read(l.store.recordRel(dir, file)); err == nil {
		_ = json.Unmarshal(b, rec)
	}
	return rec, http.StatusCreated, nil
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	wp, ok := parseWire(r.URL.Path)
	if !ok || wp.index {
		pkgbase.NotFound(w)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := l.store.Read(l.store.recordRel(wp.dir, wp.file))
	if err != nil {
		pkgbase.NotFound(w)
		return
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.deleteRecords([]*Record{&rec}); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitDeleted(rec.Path)
	w.WriteHeader(http.StatusNoContent)
}
