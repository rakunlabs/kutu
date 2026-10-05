// Package conda implements a conda channel:
//
//	GET    /channeldata.json                       channel summary
//	GET    /{subdir}/repodata.json                 package index (also current_repodata.json,
//	                                               repodata_from_packages.json)
//	GET    /{subdir}/repodata.json.zst             zstd-compressed index
//	GET    /{subdir}/{filename}                    package download (.conda / .tar.bz2)
//	PUT    /upload[/{subdir}]                      upload (local, allow_push; subdir from info/index.json wins)
//	PUT    /{subdir}/{filename}                    upload
//	DELETE /{subdir}/{filename}                    delete one package file
//
// repodata.json.bz2 is not produced locally (404; conda falls back to
// the plain or .zst index). Remote repos proxy a channel base URL such
// as https://conda.anaconda.org/conda-forge: index documents honour
// MutableTTL and are streamed through a temp file (conda-forge indexes
// are hundreds of MB), package files are cached forever. Virtual repos
// merge repodata.json / channeldata.json across members (first member
// wins on an identical filename).
//
// Client configuration:
//
//	conda config --add channels https://x:<token>@kutu.example.com/registries/{ns}/{repo}
//	curl -u x:<token> -T pkg-1.0-0.conda https://kutu.example.com/registries/{ns}/{repo}/upload/linux-64
package conda

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
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

	"github.com/klauspost/compress/zstd"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeConda

const (
	pkgsDir    = "pkgs"
	recordsDir = "records"
	indexDir   = "index"
)

const (
	extConda  = ".conda"
	extTarBz2 = ".tar.bz2"
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

var subdirRe = regexp.MustCompile(`^(noarch|[a-z0-9]+-[a-z0-9_]+)$`)

// ValidSubdir reports whether s looks like a conda platform subdir.
func ValidSubdir(s string) bool { return subdirRe.MatchString(s) }

// indexDocs are the per-subdir index documents served from index/.
var indexDocs = map[string]bool{
	"repodata.json":               true,
	"current_repodata.json":       true,
	"repodata_from_packages.json": true,
}

// SplitFilename splits "{name}-{version}-{build}{.conda|.tar.bz2}".
func SplitFilename(fn string) (name, version, build, ext string, ok bool) {
	switch {
	case strings.HasSuffix(fn, extConda):
		ext = extConda
	case strings.HasSuffix(fn, extTarBz2):
		ext = extTarBz2
	default:
		return "", "", "", "", false
	}
	stem := strings.TrimSuffix(fn, ext)
	i := strings.LastIndexByte(stem, '-')
	if i <= 0 {
		return "", "", "", "", false
	}
	build = stem[i+1:]
	stem = stem[:i]
	j := strings.LastIndexByte(stem, '-')
	if j <= 0 {
		return "", "", "", "", false
	}
	name, version = stem[:j], stem[j+1:]
	if name == "" || version == "" || build == "" {
		return "", "", "", "", false
	}
	return name, version, build, ext, true
}

func validFilename(fn string) bool {
	if fn == "" || fn == "." || fn == ".." || strings.ContainsAny(fn, "/\\\x00") {
		return false
	}
	_, _, _, _, ok := SplitFilename(fn)
	return ok
}

// ── Package metadata ──

// Record is the stored metadata of one package file.
type Record struct {
	Subdir   string         `json:"subdir"`
	Filename string         `json:"filename"`
	Index    map[string]any `json:"index"`
	About    map[string]any `json:"about,omitempty"`
	Uploaded time.Time      `json:"uploaded"`
}

func (r *Record) str(key string) string { return mapStr(r.Index, key) }
func (r *Record) about(key string) string {
	return mapStr(r.About, key)
}

func mapStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}

// Time returns the package build time (index.json timestamp) or the
// upload time.
func (r *Record) Time() time.Time {
	if ts := parseInt(r.Index["timestamp"]); ts > 0 {
		return tsToTime(ts)
	}
	return r.Uploaded
}

func tsToTime(ts int64) time.Time {
	if ts > 1e11 {
		return time.UnixMilli(ts).UTC()
	}
	return time.Unix(ts, 0).UTC()
}

func decodeMap(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// ParsePackage reads info/index.json (and info/about.json when
// present) from a .conda or .tar.bz2 package.
func ParsePackage(ext string, ra io.ReaderAt, size int64) (index, about map[string]any, err error) {
	var files map[string][]byte
	switch ext {
	case extConda:
		files, err = readConda(ra, size)
	case extTarBz2:
		files, err = readInfoTar(bzip2.NewReader(io.NewSectionReader(ra, 0, size)))
	default:
		return nil, nil, fmt.Errorf("unsupported package format %q", ext)
	}
	if err != nil {
		return nil, nil, err
	}
	raw, ok := files["info/index.json"]
	if !ok {
		return nil, nil, errors.New("package has no info/index.json")
	}
	if index, err = decodeMap(raw); err != nil {
		return nil, nil, fmt.Errorf("info/index.json: %w", err)
	}
	if b, ok := files["info/about.json"]; ok {
		about, _ = decodeMap(b)
	}
	return index, about, nil
}

func readConda(ra io.ReaderAt, size int64) (map[string][]byte, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("invalid .conda archive: %w", err)
	}
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "info-") || !strings.HasSuffix(f.Name, ".tar.zst") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		zd, err := zstd.NewReader(rc)
		if err != nil {
			return nil, err
		}
		defer zd.Close()
		return readInfoTar(zd)
	}
	return nil, errors.New(".conda archive has no info-*.tar.zst")
}

// readInfoTar collects info/index.json and info/about.json from a tar
// stream, stopping once both are found.
func readInfoTar(r io.Reader) (map[string][]byte, error) {
	tr := tar.NewReader(r)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if len(out) > 0 {
				break
			}
			return nil, fmt.Errorf("reading package tar: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name != "info/index.json" && name != "info/about.json" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, err
		}
		out[name] = b
		if len(out) == 2 {
			break
		}
	}
	return out, nil
}

// detectExt sniffs the package format from its magic bytes.
func detectExt(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return extConda
	case bytes.HasPrefix(b, []byte("BZh")):
		return extTarBz2
	}
	return ""
}

// newRecord builds a Record (index.json + checksums) for body.
func newRecord(subdir, filename string, index, about map[string]any, md5sum, sha string, size int64) *Record {
	idx := make(map[string]any, len(index)+3)
	for k, v := range index {
		idx[k] = v
	}
	idx["md5"] = md5sum
	idx["sha256"] = sha
	idx["size"] = size
	idx["subdir"] = subdir
	return &Record{Subdir: subdir, Filename: filename, Index: idx, About: about, Uploaded: time.Now().UTC()}
}

// ── Store ──

// Store wraps pkgbase.Store with the conda layout:
// pkgs/{subdir}/{file}, records/{subdir}/{file}.json, index/{subdir}/….
type Store struct{ *pkgbase.Store }

func (s *Store) pkgRel(subdir, file string) string { return path.Join(pkgsDir, subdir, file) }
func (s *Store) recordRel(subdir, file string) string {
	return path.Join(recordsDir, subdir, file+".json")
}
func (s *Store) indexRel(subdir, doc string) string { return path.Join(indexDir, subdir, doc) }

// PutRecord stores rec.
func (s *Store) PutRecord(rec *Record) error {
	b, err := pkgbase.MarshalJSON(rec)
	if err != nil {
		return err
	}
	return s.Write(s.recordRel(rec.Subdir, rec.Filename), b)
}

// Record loads the record of subdir/file.
func (s *Store) Record(subdir, file string) (*Record, error) {
	b, err := s.Read(s.recordRel(subdir, file))
	if err != nil {
		return nil, err
	}
	return decodeRecord(b)
}

func decodeRecord(b []byte) (*Record, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return nil, err
	}
	if rec.Index == nil {
		rec.Index = map[string]any{}
	}
	return &rec, nil
}

// Records returns every record of subdir ("" = all subdirs).
func (s *Store) Records(subdir string) ([]*Record, error) {
	root := recordsDir
	if subdir != "" {
		root = path.Join(recordsDir, subdir)
	}
	var out []*Record
	err := s.Walk(root, func(rel string, _ rawfs.DirEntry) error {
		if !strings.HasSuffix(rel, ".json") {
			return nil
		}
		b, err := s.Read(rel)
		if err != nil {
			return nil
		}
		if rec, err := decodeRecord(b); err == nil {
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

// Subdirs returns the subdirs that hold at least one record.
func (s *Store) Subdirs() []string {
	dirs, _ := s.ListDirs(recordsDir)
	return dirs
}

// Repodata is the repodata.json document.
type Repodata struct {
	Info            map[string]any             `json:"info"`
	Packages        map[string]json.RawMessage `json:"packages"`
	PackagesConda   map[string]json.RawMessage `json:"packages.conda"`
	Removed         []string                   `json:"removed"`
	RepodataVersion int                        `json:"repodata_version"`
}

func emptyRepodata(subdir string) *Repodata {
	return &Repodata{
		Info:            map[string]any{"subdir": subdir},
		Packages:        map[string]json.RawMessage{},
		PackagesConda:   map[string]json.RawMessage{},
		Removed:         []string{},
		RepodataVersion: 1,
	}
}

// BuildRepodata renders repodata.json for subdir from its records.
func (s *Store) BuildRepodata(subdir string) ([]byte, error) {
	recs, err := s.Records(subdir)
	if err != nil {
		return nil, err
	}
	rd := emptyRepodata(subdir)
	for _, rec := range recs {
		b, err := pkgbase.MarshalJSON(rec.Index)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(rec.Filename, extConda) {
			rd.PackagesConda[rec.Filename] = b
		} else {
			rd.Packages[rec.Filename] = b
		}
	}
	return pkgbase.MarshalJSON(rd)
}

// Reindex regenerates the stored index documents of subdir.
func (s *Store) Reindex(subdir string) error {
	body, err := s.BuildRepodata(subdir)
	if err != nil {
		return err
	}
	if err := s.Write(s.indexRel(subdir, "repodata.json"), body); err != nil {
		return err
	}
	zst, err := zstdBytes(body)
	if err != nil {
		return err
	}
	return s.Write(s.indexRel(subdir, "repodata.json.zst"), zst)
}

var zstdEnc, _ = zstd.NewWriter(nil)

func zstdBytes(b []byte) ([]byte, error) {
	return zstdEnc.EncodeAll(b, nil), nil
}

type channelPkg struct {
	Subdirs     []string `json:"subdirs"`
	Version     string   `json:"version,omitempty"`
	License     string   `json:"license,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Home        string   `json:"home,omitempty"`
	Timestamp   int64    `json:"timestamp,omitempty"`
}

// ChannelData renders channeldata.json.
func (s *Store) ChannelData() ([]byte, error) {
	recs, err := s.Records("")
	if err != nil {
		return nil, err
	}
	pkgs := map[string]*channelPkg{}
	subdirs := map[string]bool{"noarch": true}
	for _, rec := range recs {
		name := rec.str("name")
		if name == "" {
			continue
		}
		subdirs[rec.Subdir] = true
		p := pkgs[name]
		if p == nil {
			p = &channelPkg{}
			pkgs[name] = p
		}
		if !contains(p.Subdirs, rec.Subdir) {
			p.Subdirs = append(p.Subdirs, rec.Subdir)
			sort.Strings(p.Subdirs)
		}
		if v := rec.str("version"); p.Version == "" || pkgbase.CompareVersions(v, p.Version) > 0 {
			p.Version = v
			p.License = rec.str("license")
			p.Summary = rec.about("summary")
			p.Description = rec.about("description")
			p.Home = rec.about("home")
		}
		if ts := rec.Time().Unix(); ts > p.Timestamp {
			p.Timestamp = ts
		}
	}
	sd := make([]string, 0, len(subdirs))
	for k := range subdirs {
		sd = append(sd, k)
	}
	sort.Strings(sd)
	return pkgbase.MarshalJSON(map[string]any{"channeldata_version": 1, "packages": pkgs, "subdirs": sd})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ListPackages groups records by package name (versions deduped).
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	recs, err := s.Records("")
	if err != nil {
		return nil, err
	}
	set := map[string]map[string]struct{}{}
	for _, rec := range recs {
		name, ver := rec.str("name"), rec.str("version")
		if name == "" || ver == "" {
			continue
		}
		if set[name] == nil {
			set[name] = map[string]struct{}{}
		}
		set[name][ver] = struct{}{}
	}
	out := make([]registry.PackageSummary, 0, len(set))
	for name, vs := range set {
		versions := make([]string, 0, len(vs))
		for v := range vs {
			versions = append(versions, v)
		}
		pkgbase.SortVersions(versions)
		out = append(out, registry.PackageSummary{Name: name, Versions: versions})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// versionRecords returns the records of name@version ("" = all versions).
func (s *Store) versionRecords(name, version string) ([]*Record, error) {
	recs, err := s.Records("")
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, rec := range recs {
		if rec.str("name") == name && (version == "" || rec.str("version") == version) {
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
		v := rec.str("version")
		if _, ok := byVer[v]; !ok {
			versions = append(versions, v)
		}
		byVer[v] = append(byVer[v], rec)
	}
	pkgbase.SortVersions(versions)
	latest := versions[len(versions)-1]
	d := &registry.GenericPackageDetail{LatestVersion: latest}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		row := registry.GenericVersionDetail{Version: v}
		var newest time.Time
		var subdirs, builds []string
		for _, rec := range byVer[v] {
			size := parseInt(rec.Index["size"])
			row.Files = append(row.Files, registry.GenericFile{Name: rec.Subdir + "/" + rec.Filename, Size: size, SHA256: rec.str("sha256")})
			row.Size += size
			if t := rec.Time(); t.After(newest) {
				newest = t
			}
			if !contains(subdirs, rec.Subdir) {
				subdirs = append(subdirs, rec.Subdir)
			}
			if b := rec.str("build"); !contains(builds, b) {
				builds = append(builds, b)
			}
		}
		sort.Slice(row.Files, func(a, b int) bool { return row.Files[a].Name < row.Files[b].Name })
		sort.Strings(subdirs)
		sort.Strings(builds)
		row.Metadata = map[string]string{"subdirs": strings.Join(subdirs, ","), "builds": strings.Join(builds, ",")}
		if !newest.IsZero() {
			row.PublishedAt = newest.Format(time.RFC3339)
		}
		d.Versions = append(d.Versions, row)
	}
	top := byVer[latest][0]
	d.License = top.str("license")
	d.Description = top.about("summary")
	if d.Description == "" {
		d.Description = top.about("description")
	}
	d.Homepage = top.about("home")
	if dev := top.about("dev_url"); dev != "" {
		d.Metadata = map[string]string{"dev_url": dev}
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	recs, err := s.versionRecords(ref.Name, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	if len(recs) == 0 || ref.Version == "" {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	var m registry.ArtifactMeta
	for _, rec := range recs {
		if m.License == "" {
			m.License = rec.str("license")
		}
		if t := rec.Time(); m.PublishedAt.IsZero() || t.Before(m.PublishedAt) {
			m.PublishedAt = t
		}
	}
	return m, nil
}

// splitPath returns the two segments of "/{subdir}/{file}".
func splitPath(p string) (subdir, file string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 2 || !ValidSubdir(parts[0]) || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	_, file, ok := splitPath(r.URL.Path)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	name, version, _, _, ok := SplitFilename(file)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: name, Version: version}, true
}

func contentTypeFor(file string) string {
	switch {
	case strings.HasSuffix(file, ".json"):
		return "application/json"
	case strings.HasSuffix(file, ".zst"):
		return "application/zstd"
	case strings.HasSuffix(file, ".bz2"):
		return "application/x-bzip2"
	}
	return "application/octet-stream"
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

// DeleteVersion removes every build of name@version in every subdir.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	recs, err := l.store.versionRecords(name, version)
	if err != nil {
		return err
	}
	if len(recs) == 0 || version == "" {
		return registry.ErrPackageNotFound
	}
	touched := map[string]bool{}
	for _, rec := range recs {
		if err := l.deleteFile(rec.Subdir, rec.Filename); err != nil {
			return err
		}
		touched[rec.Subdir] = true
	}
	for sd := range touched {
		if err := l.store.Reindex(sd); err != nil {
			return err
		}
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

func (l *Local) deleteFile(subdir, file string) error {
	if err := l.store.Delete(l.store.pkgRel(subdir, file)); err != nil {
		return err
	}
	return l.store.Delete(l.store.recordRel(subdir, file))
}

// PromoteVersion copies every file of name@version into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("conda: promote target %T is not a conda local registry", dst)
	}
	recs, err := l.store.versionRecords(name, version)
	if err != nil {
		return err
	}
	if len(recs) == 0 || version == "" {
		return registry.ErrPackageNotFound
	}
	for _, rec := range recs {
		body, err := l.store.Read(l.store.pkgRel(rec.Subdir, rec.Filename))
		if err != nil {
			return fmt.Errorf("conda: read %s/%s: %w", rec.Subdir, rec.Filename, err)
		}
		if _, _, err := d.publish(rec.Subdir, rec.Filename, body); err != nil {
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
	p := strings.Trim(r.URL.Path, "/")
	if p == "channeldata.json" {
		body, err := l.store.ChannelData()
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", body)
		return
	}
	subdir, file, ok := splitPath(p)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	switch {
	case indexDocs[file]:
		if pkgbase.ServeStored(w, r, l.store.Store, l.store.indexRel(subdir, "repodata.json"), "application/json") {
			return
		}
		body, err := l.store.BuildRepodata(subdir)
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", body)
	case file == "repodata.json.zst":
		if pkgbase.ServeStored(w, r, l.store.Store, l.store.indexRel(subdir, file), "application/zstd") {
			return
		}
		body, err := l.store.BuildRepodata(subdir)
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		zst, _ := zstdBytes(body)
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/zstd", zst)
	case validFilename(file):
		if !pkgbase.ServeStored(w, r, l.store.Store, l.store.pkgRel(subdir, file), "application/octet-stream") {
			pkgbase.NotFound(w)
		}
	default:
		pkgbase.NotFound(w)
	}
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	var subdir, file string
	switch {
	case len(parts) == 1 && parts[0] == "upload":
	case len(parts) == 2 && parts[0] == "upload" && ValidSubdir(parts[1]):
		subdir = parts[1]
	case len(parts) == 2 && ValidSubdir(parts[0]) && validFilename(parts[1]):
		subdir, file = parts[0], parts[1]
	default:
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /upload/{subdir} or PUT /{subdir}/{filename}")
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	rec, code, err := l.publish(subdir, file, body)
	if err != nil {
		pkgbase.Error(w, code, err.Error())
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"name": rec.str("name"), "version": rec.str("version"), "build": rec.str("build"),
		"subdir": rec.Subdir, "filename": rec.Filename, "size": len(body), "sha256": rec.str("sha256"),
	})
}

// publish stores one package file and regenerates its subdir index.
// subdir and file are hints; the package's index.json wins.
func (l *Local) publish(subdir, file string, body []byte) (*Record, int, error) {
	ext := detectExt(body)
	if ext == "" {
		return nil, http.StatusBadRequest, errors.New("body is not a .conda or .tar.bz2 package")
	}
	if file != "" && !strings.HasSuffix(file, ext) {
		return nil, http.StatusBadRequest, fmt.Errorf("filename %q does not match package format %s", file, ext)
	}
	index, about, err := ParsePackage(ext, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	name, version, build := mapStr(index, "name"), mapStr(index, "version"), mapStr(index, "build")
	if name == "" || version == "" || build == "" {
		return nil, http.StatusBadRequest, errors.New("info/index.json must define name, version and build")
	}
	if sd := mapStr(index, "subdir"); sd != "" {
		subdir = sd
	} else if subdir == "" && index["noarch"] != nil {
		subdir = "noarch"
	}
	if !ValidSubdir(subdir) {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid or missing subdir %q", subdir)
	}
	file = name + "-" + version + "-" + build + ext
	if !validFilename(file) {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid package filename %q", file)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	rel := l.store.pkgRel(subdir, file)
	if code, err := l.Guard.Check(l.store.Store, l.store.Exists(rel), int64(len(body))); err != nil {
		return nil, code, err
	}
	sum := md5.Sum(body)
	rec := newRecord(subdir, file, index, about, hex.EncodeToString(sum[:]), pkgbase.SHA256Hex(body), int64(len(body)))
	if err := l.store.Write(rel, body); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := l.store.PutRecord(rec); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := l.store.Reindex(subdir); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	l.EmitPublished(subdir+"/"+file, int64(len(body)))
	// Re-read so numeric fields carry json.Number like stored records.
	if stored, err := l.store.Record(subdir, file); err == nil {
		rec = stored
	}
	return rec, http.StatusCreated, nil
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	subdir, file, ok := splitPath(r.URL.Path)
	if !ok || !validFilename(file) || !l.store.Exists(l.store.pkgRel(subdir, file)) {
		pkgbase.NotFound(w)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.deleteFile(subdir, file); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.Reindex(subdir); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitDeleted(subdir + "/" + file)
	w.WriteHeader(http.StatusNoContent)
}

// parseInt is a small helper for json.Number / string fields.
func parseInt(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}
