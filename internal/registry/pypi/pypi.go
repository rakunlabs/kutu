// Package pypi implements a Python package index (local, remote and
// virtual repositories).
//
// Endpoints (relative to /registries/{ns}/{repo}):
//
//	GET    /simple/                          PEP 503/691 project list (HTML or JSON)
//	GET    /simple/{name}/                   PEP 503/691 project page (HTML or JSON)
//	GET    /packages/{name}/{file}           distribution download (local)
//	GET    /packages/{name}/{file}.metadata  PEP 658 core metadata (local wheels)
//	GET    /_remote/{key}/{file}[.metadata]  cached upstream file (remote)
//	GET    /pypi/{name}/json                 warehouse-compatible JSON API
//	GET    /pypi/{name}/{version}/json       warehouse-compatible JSON API (one release)
//	POST   /  or /legacy                     twine upload (multipart)
//	PUT    /packages/{name}/{file}           raw upload
//	DELETE /packages/{name}/{file}           delete one file
//	POST   /yank  /unyank                    PEP 592 yank (form: name, version, reason[, filename])
//
// The Simple API negotiates on Accept (application/vnd.pypi.simple.v1+json,
// application/vnd.pypi.simple.v1+html, text/html) or ?format=.
//
// Client configuration:
//
//	pip install --index-url https://<user>:<token>@kutu.example.com/registries/<ns>/<repo>/simple/ pkg
//	twine upload --repository-url https://kutu.example.com/registries/<ns>/<repo>/ -u kutu -p <token> dist/*
package pypi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/events"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const metadataSuffix = ".metadata"

// Store is the rawfs-backed PyPI layout:
//
//	packages/{name}/{file}            distributions (local)
//	metadata/{name}/{file}.metadata   PEP 658 core metadata (local)
//	meta/{name}.json                  per-package sidecar (local)
//	simple/{name}.json                cached upstream index (remote)
//	pypi-json/...                     cached upstream JSON API (remote)
//	remote/{key}[.metadata]           cached upstream files (remote)
type Store struct {
	fs       rawfs.RawFS
	basePath string
	mu       sync.Mutex
}

func NewStore(fs rawfs.RawFS, basePath string) *Store {
	return &Store{fs: fs, basePath: strings.Trim(basePath, "/")}
}

func (s *Store) RawFS() rawfs.RawFS { return s.fs }

func (s *Store) join(parts ...string) string {
	cleaned := make([]string, 0, len(parts)+1)
	if s.basePath != "" {
		cleaned = append(cleaned, s.basePath)
	}
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return path.Join(cleaned...)
}

func (s *Store) readRaw(abs string) ([]byte, error) {
	rc, _, err := s.fs.Open(abs)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (s *Store) writeRaw(abs string, body []byte) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("pypi: backend read-only")
	}
	return wfs.Write(abs, bytes.NewReader(body), int64(len(body)))
}

func (s *Store) deleteRaw(abs string) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("pypi: backend read-only")
	}
	if err := wfs.Delete(abs); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (s *Store) exists(abs string) bool {
	_, err := s.fs.Stat(abs)
	return err == nil
}

func normalizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return normalizeRE.ReplaceAllString(name, "-")
}

var normalizeRE = regexp.MustCompile(`[-_.]+`)
var filenameVersionRE = regexp.MustCompile(`^(.+?)-([0-9][A-Za-z0-9.!+_]*)(?:-|$)`)

func InferNameVersion(filename string) (string, string) {
	base := path.Base(filename)
	for _, suffix := range []string{".tar.gz", ".whl", ".zip", ".tar.bz2", ".tgz"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if m := filenameVersionRE.FindStringSubmatch(base); len(m) == 3 {
		return normalizeName(m[1]), m[2]
	}
	return "", ""
}

// versionFiles groups the stored files of name by version (sidecar
// version when recorded, filename inference otherwise).
func (s *Store) versionFiles(name string) (map[string][]File, error) {
	files, err := s.ListPackageFiles(name)
	if err != nil {
		return nil, err
	}
	pm, _ := s.LoadMeta(name)
	out := map[string][]File{}
	for _, f := range files {
		if v := fileVersion(pm, f.Name); v != "" {
			out[v] = append(out[v], f)
		}
	}
	return out, nil
}

func (s *Store) ListVersions(name string) ([]string, error) {
	byVer, err := s.versionFiles(name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(byVer))
	for v := range byVer {
		out = append(out, v)
	}
	pkgbase.SortVersions(out)
	return out, nil
}

func (s *Store) packagePath(name, filename string) string {
	return s.join("packages", normalizeName(name), path.Base(filename))
}

func (s *Store) remotePath(encoded string) string { return s.join("remote", encoded) }
func (s *Store) simplePath(name string) string    { return s.join("simple", normalizeName(name)+".json") }

func (s *Store) WritePackage(name, filename string, body []byte) error {
	return s.writeRaw(s.packagePath(name, filename), body)
}

func (s *Store) OpenPackage(name, filename string) (rawfs.ReadSeekCloser, *rawfs.FileInfo, error) {
	rc, fi, err := s.fs.Open(s.packagePath(name, filename))
	if err != nil {
		return nil, nil, err
	}
	return rc, fi, nil
}

func (s *Store) ListPackages() ([]string, error) {
	entries, err := s.fs.ReadDir(s.join("packages"))
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			files, _ := s.ListPackageFiles(e.Name)
			if len(files) == 0 {
				continue
			}
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

type File struct {
	Name string
	Size int64
}

func (s *Store) ListPackageFiles(name string) ([]File, error) {
	entries, err := s.fs.ReadDir(s.join("packages", normalizeName(name)))
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []File
	for _, e := range entries {
		if !e.IsDir {
			out = append(out, File{Name: e.Name, Size: e.Size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FileMetas returns the sidecar records of every stored file of name,
// sorted by filename. Files uploaded before sidecars existed are
// backfilled (hash, size, mtime) and persisted.
func (s *Store) FileMetas(name string) ([]*FileMeta, error) {
	name = normalizeName(name)
	files, err := s.ListPackageFiles(name)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pm, err := s.LoadMeta(name)
	if err != nil {
		return nil, err
	}
	dirty := false
	present := map[string]bool{}
	out := make([]*FileMeta, 0, len(files))
	for _, f := range files {
		present[f.Name] = true
		fm := pm.Files[f.Name]
		if fm == nil || fm.SHA256 == "" {
			rc, fi, err := s.OpenPackage(name, f.Name)
			if err != nil {
				continue
			}
			body, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				continue
			}
			prev := fm
			fm, _, _ = buildFileMeta(f.Name, body, nil, fi.ModTime)
			if prev != nil {
				fm.Yanked, fm.YankedReason = prev.Yanked, prev.YankedReason
			}
			if fi.ModTime.IsZero() {
				fm.UploadTime = time.Time{}
			}
			pm.Files[f.Name] = fm
			dirty = true
		}
		out = append(out, fm)
	}
	for fn := range pm.Files {
		if !present[fn] {
			delete(pm.Files, fn)
			dirty = true
		}
	}
	if dirty {
		_ = s.saveMeta(name, pm)
	}
	return out, nil
}

// OpenMetadata opens the PEP 658 metadata stored for a distribution.
func (s *Store) OpenMetadata(name, filename string) (rawfs.ReadSeekCloser, *rawfs.FileInfo, error) {
	return s.fs.Open(s.metadataPath(name, filename))
}

// DeleteFile removes one distribution file from a package.
func (s *Store) DeleteFile(name, filename string) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("pypi: backend read-only")
	}
	if err := wfs.Delete(s.packagePath(name, filename)); err != nil {
		if isNotFound(err) {
			return registry.ErrPackageNotFound
		}
		return err
	}
	_ = s.deleteRaw(s.metadataPath(name, filename))
	_ = s.UpdateMeta(name, func(pm *PackageMeta) error {
		delete(pm.Files, path.Base(filename))
		return nil
	})
	return nil
}

// DeleteVersion removes every distribution file belonging to a
// version. PyPI can publish multiple files per version (wheel,
// sdist), so deletion is version-wide rather than file-system-wide.
func (s *Store) DeleteVersion(name, version string) (int, error) {
	byVer, err := s.versionFiles(name)
	if err != nil {
		return 0, err
	}
	var deleted int
	var firstErr error
	for _, f := range byVer[version] {
		if err := s.DeleteFile(name, f.Name); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		deleted++
	}
	if deleted == 0 && firstErr == nil {
		return 0, registry.ErrPackageNotFound
	}
	return deleted, firstErr
}

// SetYanked marks (or clears) the yank flag on every file of version,
// or on one file when filename is non-empty. Returns the files changed.
func (s *Store) SetYanked(name, version, filename string, yanked bool, reason string) (int, error) {
	name = normalizeName(name)
	if _, err := s.FileMetas(name); err != nil {
		return 0, err
	}
	var n int
	err := s.UpdateMeta(name, func(pm *PackageMeta) error {
		for fn, fm := range pm.Files {
			if filename != "" && fn != path.Base(filename) {
				continue
			}
			if version != "" && fm.Version != version {
				continue
			}
			fm.Yanked = yanked
			fm.YankedReason = ""
			if yanked {
				fm.YankedReason = reason
			}
			n++
		}
		if n == 0 {
			return registry.ErrPackageNotFound
		}
		return nil
	})
	return n, err
}

func (s *Store) Count() (packages, versions, files int, bytes int64) {
	names, _ := s.ListPackages()
	packages = len(names)
	for _, name := range names {
		byVer, _ := s.versionFiles(name)
		versions += len(byVer)
		fs, _ := s.ListPackageFiles(name)
		files += len(fs)
		for _, f := range fs {
			bytes += f.Size
		}
	}
	return
}

func (s *Store) WriteRemote(encoded string, body []byte) error {
	return s.writeRaw(s.remotePath(encoded), body)
}

func (s *Store) OpenRemote(encoded string) (rawfs.ReadSeekCloser, *rawfs.FileInfo, error) {
	return s.fs.Open(s.remotePath(encoded))
}

// WriteSimple caches a remote index document (internal JSON form).
func (s *Store) WriteSimple(name string, body []byte) error {
	return s.writeRaw(s.simplePath(name), body)
}

func (s *Store) OpenSimple(name string) (rawfs.ReadSeekCloser, *rawfs.FileInfo, error) {
	return s.fs.Open(s.simplePath(name))
}

func (s *Store) PurgeAll() (int, int64, []error) { return s.purge("") }

func (s *Store) PurgeMutable() (int, int64, []error) {
	c1, b1, e1 := s.purge("simple")
	c2, b2, e2 := s.purge("pypi-json")
	return c1 + c2, b1 + b2, append(e1, e2...)
}

func (s *Store) purge(prefix string) (int, int64, []error) {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return 0, 0, []error{fmt.Errorf("pypi: backend read-only")}
	}
	root := s.join(prefix)
	var count int
	var bytes int64
	var errs []error
	var walk func(abs string)
	walk = func(abs string) {
		entries, err := s.fs.ReadDir(abs)
		if err != nil {
			if !isNotFound(err) {
				errs = append(errs, err)
			}
			return
		}
		for _, e := range entries {
			child := path.Join(abs, e.Name)
			if e.IsDir {
				walk(child)
				continue
			}
			if err := wfs.Delete(child); err != nil && !isNotFound(err) {
				errs = append(errs, err)
				continue
			}
			count++
			bytes += e.Size
		}
	}
	walk(root)
	return count, bytes, errs
}

// ── Local ──

type Local struct {
	namespace string
	name      string
	store     *Store
	allowPush bool
	maxUpload int64
	emitter   events.Emitter
	guard     pkgbase.PublishGuard
	base      *pkgbase.Store
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		fs, err := deps.MountRawFS(r.Mount)
		if err != nil {
			return nil, fmt.Errorf("pypi/local %s/%s: %w", ns, r.Name, err)
		}
		return &Local{
			namespace: ns, name: r.Name, store: NewStore(fs, r.BasePath), allowPush: r.AllowPush,
			maxUpload: r.MaxUploadSize, emitter: deps.Emitter, guard: pkgbase.GuardFor(r), base: pkgbase.NewStore(fs, r.BasePath),
		}, nil
	}
}

func (l *Local) Namespace() string { return l.namespace }
func (l *Local) Name() string      { return l.name }
func (l *Local) Type() string      { return service.RegistryTypePyPI }
func (l *Local) Kind() string      { return service.RegistryKindLocal }
func (l *Local) Store() *Store     { return l.store }
func (l *Local) Close() error      { return nil }

func (l *Local) Stats(context.Context) (registry.Stats, error) {
	packages, versions, files, bytes := l.store.Count()
	return registry.Stats{PackageCount: packages, VersionCount: versions, BlobCount: files, TotalBytes: bytes}, nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return packageDetail(l.store, name)
}

// ListPackages implements registry.PackageLister.
func (l *Local) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := l.store.ListPackages()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(names))
	for _, n := range names {
		vs, _ := l.store.ListVersions(n)
		out = append(out, registry.PackageSummary{Name: n, Versions: vs})
	}
	return out, nil
}

// DeleteVersion implements registry.VersionDeleter.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	if _, err := l.store.DeleteVersion(name, version); err != nil {
		return err
	}
	l.emit(hook.EventRegistryDeleted, normalizeName(name)+"@"+version, 0)
	return nil
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	p := strings.Trim(r.URL.Path, "/")
	if rest, ok := strings.CutPrefix(p, "packages/"); ok {
		name, file, ok := strings.Cut(rest, "/")
		if !ok || name == "" || file == "" {
			return registry.ArtifactRef{}, false
		}
		name = normalizeName(name)
		if strings.HasSuffix(file, metadataSuffix) {
			return registry.ArtifactRef{Name: name}, true
		}
		pm, _ := l.store.LoadMeta(name)
		return registry.ArtifactRef{Name: name, Version: fileVersion(pm, file)}, true
	}
	return classifyCommon(p)
}

// classifyCommon handles the metadata routes shared by every kind.
func classifyCommon(p string) (registry.ArtifactRef, bool) {
	if rest, ok := strings.CutPrefix(p, "simple/"); ok && rest != "" {
		name, _, _ := strings.Cut(rest, "/")
		return registry.ArtifactRef{Name: normalizeName(name)}, true
	}
	if rest, ok := strings.CutPrefix(p, "pypi/"); ok {
		name, _, _ := strings.Cut(rest, "/")
		if name != "" {
			return registry.ArtifactRef{Name: normalizeName(name)}, true
		}
	}
	return registry.ArtifactRef{}, false
}

// ArtifactInfo implements registry.ArtifactInfoProvider.
func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	metas, err := l.store.FileMetas(ref.Name)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	var out registry.ArtifactMeta
	found := false
	for _, fm := range metas {
		if fm.Version != ref.Version {
			continue
		}
		found = true
		if out.License == "" {
			out.License = fm.License
		}
		if !fm.UploadTime.IsZero() && (out.PublishedAt.IsZero() || fm.UploadTime.Before(out.PublishedAt)) {
			out.PublishedAt = fm.UploadTime
		}
	}
	if !found {
		return out, registry.ErrPackageNotFound
	}
	return out, nil
}

// PromoteVersion implements registry.VersionPromoter.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("pypi: promote target %s/%s is not a local pypi repository", dst.Namespace(), dst.Name())
	}
	name = normalizeName(name)
	metas, err := l.store.FileMetas(name)
	if err != nil {
		return err
	}
	var copied []*FileMeta
	for _, fm := range metas {
		if fm.Version != version {
			continue
		}
		body, err := l.store.readRaw(l.store.packagePath(name, fm.Filename))
		if err != nil {
			return err
		}
		if err := d.store.WritePackage(name, fm.Filename, body); err != nil {
			return err
		}
		if core, err := l.store.readRaw(l.store.metadataPath(name, fm.Filename)); err == nil {
			if err := d.store.writeRaw(d.store.metadataPath(name, fm.Filename), core); err != nil {
				return err
			}
		}
		cp := *fm
		copied = append(copied, &cp)
		d.emit(hook.EventRegistryPublished, name+"/"+fm.Filename, fm.Size)
	}
	if len(copied) == 0 {
		return registry.ErrPackageNotFound
	}
	return d.store.UpdateMeta(name, func(pm *PackageMeta) error {
		for _, fm := range copied {
			pm.Files[fm.Filename] = fm
		}
		return nil
	})
}

func (l *Local) emit(typ hook.EventType, subject string, size int64) {
	events.EmitSafe(l.emitter, hook.Event{Type: typ, Mount: l.namespace, Path: l.name + "/" + subject, Protocol: "registry-pypi", Size: size})
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSuffix(r.URL.Path, "/")
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case read && (p == "/simple" || p == ""):
		l.serveSimpleRoot(w, r)
	case read && strings.HasPrefix(p, "/simple/"):
		l.serveSimplePackage(w, r, strings.TrimPrefix(p, "/simple/"))
	case read && strings.HasPrefix(p, "/packages/"):
		l.servePackageFile(w, r)
	case read && strings.HasPrefix(p, "/pypi/") && strings.HasSuffix(p, "/json"):
		l.serveJSONAPI(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/pypi/"), "/json"))
	case r.Method == http.MethodPost && (p == "" || p == "/legacy"):
		l.publishMultipart(w, r)
	case r.Method == http.MethodPost && (p == "/yank" || p == "/unyank"):
		l.yank(w, r, p == "/yank")
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/packages/"):
		l.publishPut(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/packages/"):
		l.deleteFile(w, r)
	default:
		http.Error(w, "no pypi route", http.StatusNotFound)
	}
}

func (l *Local) serveSimpleRoot(w http.ResponseWriter, r *http.Request) {
	names, _ := l.store.ListPackages()
	writeIndex(w, r, &index{Projects: names, LinkBase: pkgbase.Prefix(r) + "/simple/"})
}

func (l *Local) localIndex(r *http.Request, name string) *index {
	metas, _ := l.store.FileMetas(name)
	if len(metas) == 0 {
		return nil
	}
	prefix := pkgbase.Prefix(r)
	idx := &index{Name: name}
	seen := map[string]bool{}
	for _, fm := range metas {
		f := indexFile{
			Filename:       fm.Filename,
			URL:            prefix + "/packages/" + url.PathEscape(name) + "/" + url.PathEscape(fm.Filename),
			Hashes:         map[string]string{"sha256": fm.SHA256},
			RequiresPython: fm.RequiresPython,
			Yanked:         fm.Yanked,
			YankedReason:   fm.YankedReason,
			Size:           fm.Size,
			UploadTime:     formatUploadTime(fm.UploadTime),
		}
		if fm.CoreMetadataSHA256 != "" {
			f.HasCoreMetadata = true
			f.CoreMetadata = map[string]string{"sha256": fm.CoreMetadataSHA256}
		}
		idx.Files = append(idx.Files, f)
		if fm.Version != "" && !seen[fm.Version] {
			seen[fm.Version] = true
			idx.Versions = append(idx.Versions, fm.Version)
		}
	}
	pkgbase.SortVersions(idx.Versions)
	return idx
}

func (l *Local) serveSimplePackage(w http.ResponseWriter, r *http.Request, name string) {
	idx := l.localIndex(r, normalizeName(name))
	if idx == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeIndex(w, r, idx)
}

func (l *Local) servePackageFile(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/packages/"), "/")
	if len(parts) < 2 {
		http.Error(w, "expected /packages/{name}/{filename}", http.StatusBadRequest)
		return
	}
	rc, fi, err := l.store.OpenPackage(parts[0], parts[1])
	if err != nil && strings.HasSuffix(parts[1], metadataSuffix) {
		if mrc, mfi, merr := l.store.OpenMetadata(parts[0], strings.TrimSuffix(parts[1], metadataSuffix)); merr == nil {
			defer mrc.Close()
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			writeBody(w, r, mrc, mfi)
			return
		}
	}
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer rc.Close()
	writeFile(w, r, parts[1], rc, fi)
}

func (l *Local) maxBody() int64 {
	if l.maxUpload == 0 {
		return 512 * 1024 * 1024
	}
	return l.maxUpload
}

func (l *Local) publishMultipart(w http.ResponseWriter, r *http.Request) {
	if !l.allowPush {
		http.Error(w, "push disabled", http.StatusMethodNotAllowed)
		return
	}
	max := l.maxBody()
	if err := r.ParseMultipartForm(max); err != nil {
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("content")
	if err != nil {
		http.Error(w, "missing content file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	body, err := readLimited(file, max)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		name, _ = InferNameVersion(hdr.Filename)
	}
	if name == "" {
		http.Error(w, "missing package name", http.StatusBadRequest)
		return
	}
	l.finishPublish(w, normalizeName(name), hdr.Filename, body, r.Form)
}

func (l *Local) publishPut(w http.ResponseWriter, r *http.Request) {
	if !l.allowPush {
		http.Error(w, "push disabled", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/packages/"), "/")
	var name, filename string
	if len(parts) >= 2 {
		name, filename = parts[0], parts[1]
	} else if len(parts) == 1 {
		filename = parts[0]
		name, _ = InferNameVersion(filename)
	}
	if name == "" || filename == "" || strings.HasSuffix(filename, metadataSuffix) {
		http.Error(w, "expected /packages/{name}/{filename}", http.StatusBadRequest)
		return
	}
	body, err := readLimited(r.Body, l.maxBody())
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	l.finishPublish(w, normalizeName(name), filename, body, nil)
}

func (l *Local) finishPublish(w http.ResponseWriter, name, filename string, body []byte, form url.Values) {
	filename = path.Base(filename)
	fm, core, err := buildFileMeta(filename, body, form, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	exists := l.store.exists(l.store.packagePath(name, filename))
	if !l.guard.Allow(w, l.base, exists, int64(len(body))) {
		return
	}
	if err := l.store.WritePackage(name, filename, body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if core != nil {
		_ = l.store.writeRaw(l.store.metadataPath(name, filename), core)
	} else {
		_ = l.store.deleteRaw(l.store.metadataPath(name, filename))
	}
	if err := l.store.UpdateMeta(name, func(pm *PackageMeta) error {
		pm.Files[filename] = fm
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	l.emit(hook.EventRegistryPublished, name+"/"+filename, int64(len(body)))
	w.WriteHeader(http.StatusCreated)
}

func (l *Local) yank(w http.ResponseWriter, r *http.Request, yanked bool) {
	if !l.allowPush {
		http.Error(w, "push disabled", http.StatusMethodNotAllowed)
		return
	}
	name := normalizeName(r.FormValue("name"))
	version := strings.TrimSpace(r.FormValue("version"))
	filename := strings.TrimSpace(r.FormValue("filename"))
	if name == "" || (version == "" && filename == "") {
		http.Error(w, "name and version (or filename) are required", http.StatusBadRequest)
		return
	}
	n, err := l.store.SetYanked(name, version, filename, yanked, strings.TrimSpace(r.FormValue("reason")))
	if err != nil {
		if errors.Is(err, registry.ErrPackageNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"name": name, "version": version, "yanked": yanked, "files": n})
}

func (l *Local) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !l.allowPush {
		http.Error(w, "delete disabled", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/packages/"), "/")
	if len(parts) < 2 {
		http.Error(w, "expected /packages/{name}/{filename}", http.StatusBadRequest)
		return
	}
	if err := l.store.DeleteFile(parts[0], parts[1]); err != nil && !errors.Is(err, registry.ErrPackageNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	l.emit(hook.EventRegistryDeleted, normalizeName(parts[0])+"/"+parts[1], 0)
	w.WriteHeader(http.StatusNoContent)
}

func (l *Local) serveJSONAPI(w http.ResponseWriter, r *http.Request, rest string) {
	name, version, _ := strings.Cut(rest, "/")
	name = normalizeName(name)
	metas, _ := l.store.FileMetas(name)
	if len(metas) == 0 {
		pkgbase.NotFound(w)
		return
	}
	doc := buildJSONAPI(pkgbase.PublicBase(r), name, version, metas)
	if doc == nil {
		pkgbase.NotFound(w)
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, doc)
}

// ── Remote ──

type Remote struct {
	namespace  string
	name       string
	store      *Store
	client     *upstream.Client
	mutableTTL time.Duration
	base       *pkgbase.Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		b, err := upstream.BuildRemote(deps, "pypi/remote", ns, r, 5*time.Minute)
		if err != nil {
			return nil, err
		}
		return &Remote{namespace: ns, name: r.Name, store: NewStore(b.FS, b.BasePath), client: b.Client, mutableTTL: b.MutableTTL, base: pkgbase.NewStore(b.FS, b.BasePath)}, nil
	}
}

func (rr *Remote) Namespace() string { return rr.namespace }
func (rr *Remote) Name() string      { return rr.name }
func (rr *Remote) Type() string      { return service.RegistryTypePyPI }
func (rr *Remote) Kind() string      { return service.RegistryKindRemote }
func (rr *Remote) Store() *Store     { return rr.store }
func (rr *Remote) Close() error {
	if rr.client != nil {
		return rr.client.Close()
	}
	return nil
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.base), nil
}

// PackageDetail reports the cached upstream project page.
func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	idx := rr.cachedIndex(normalizeName(name))
	if idx == nil || len(idx.Files) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	byVersion := map[string]*registry.PyPIVersionDetail{}
	for _, f := range idx.Files {
		_, v := InferNameVersion(f.Filename)
		if v == "" {
			v = "unknown"
		}
		row := byVersion[v]
		if row == nil {
			row = &registry.PyPIVersionDetail{Version: v}
			byVersion[v] = row
		}
		row.Files = append(row.Files, f.Filename)
		row.FileSize += f.Size
	}
	return detailFrom(normalizeName(name), byVersion), nil
}

// ListPackages lists the projects whose index page is cached.
func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	files, err := rr.base.ListFiles("simple")
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, f := range files {
		name, ok := strings.CutSuffix(f.Name, ".json")
		if !ok || name == "_root" {
			continue
		}
		idx := rr.cachedIndex(name)
		if idx == nil {
			continue
		}
		out = append(out, registry.PackageSummary{Name: name, Versions: idx.Versions})
	}
	return out, nil
}

func (rr *Remote) ProbeUpstream(ctx context.Context) (registry.UpstreamHealth, error) {
	return upstream.Probe(ctx, rr.client, "/simple/"), nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	count, bytes, errs := rr.store.PurgeMutable()
	if opts.All {
		count, bytes, errs = rr.store.PurgeAll()
	}
	out := registry.PurgeStats{PurgedFiles: count, PurgedBytes: bytes}
	for _, err := range errs {
		out.Errors = append(out.Errors, err.Error())
	}
	return out, nil
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	p := strings.Trim(r.URL.Path, "/")
	if rest, ok := strings.CutPrefix(p, "_remote/"); ok {
		key, file, meta := splitRemoteKey(rest)
		if file == "" {
			raw, err := base64.RawURLEncoding.DecodeString(key)
			if err != nil {
				return registry.ArtifactRef{}, false
			}
			file = pathBase(string(raw))
		}
		name, version := InferNameVersion(file)
		if name == "" {
			return registry.ArtifactRef{}, false
		}
		if meta {
			version = ""
		}
		return registry.ArtifactRef{Name: name, Version: version}, true
	}
	return classifyCommon(p)
}

// ArtifactInfo reads license and upload time from the upstream JSON
// API, falling back to the Simple API upload-time.
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	name := normalizeName(ref.Name)
	var out registry.ArtifactMeta
	if body, err := rr.fetchJSONAPI(ctx, name, ref.Version); err == nil {
		if doc, err := decodeJSONAPI(body); err == nil {
			out.License = doc.license()
			out.PublishedAt = doc.published(ref.Version)
			if out.License != "" || !out.PublishedAt.IsZero() {
				return out, nil
			}
		}
	}
	idx, err := rr.fetchIndex(ctx, name)
	if err != nil {
		return out, registry.ErrPackageNotFound
	}
	found := false
	for _, f := range idx.Files {
		if _, v := InferNameVersion(f.Filename); v != ref.Version {
			continue
		}
		found = true
		if t, err := time.Parse(time.RFC3339, f.UploadTime); err == nil && (out.PublishedAt.IsZero() || t.Before(out.PublishedAt)) {
			out.PublishedAt = t
		}
	}
	if !found {
		return out, registry.ErrPackageNotFound
	}
	return out, nil
}

// Prefetch warms the project page and every file (plus PEP 658
// metadata) of version, or of the latest non-yanked version.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	idx, err := rr.fetchIndex(ctx, normalizeName(name))
	if err != nil {
		return err
	}
	if version == "" {
		version = latestVersion(idx)
	}
	n := 0
	for _, f := range idx.Files {
		if _, v := InferNameVersion(f.Filename); v != version {
			continue
		}
		key := encodeKey(f.URL)
		if _, err := rr.fetchRemote(ctx, key, false); err != nil {
			return err
		}
		if f.HasCoreMetadata {
			_, _ = rr.fetchRemote(ctx, key, true)
		}
		n++
	}
	if n == 0 {
		return registry.ErrPackageNotFound
	}
	return nil
}

func latestVersion(idx *index) string {
	yanked := map[string]bool{}
	all := map[string]bool{}
	for _, f := range idx.Files {
		_, v := InferNameVersion(f.Filename)
		if v == "" {
			continue
		}
		if _, ok := all[v]; !ok {
			yanked[v] = true
		}
		all[v] = true
		if !f.Yanked {
			yanked[v] = false
		}
	}
	var live, any []string
	for v := range all {
		any = append(any, v)
		if !yanked[v] {
			live = append(live, v)
		}
	}
	if l := pkgbase.Latest(live); l != "" {
		return l
	}
	return pkgbase.Latest(any)
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "remote registry is read-only", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case p == "/simple" || p == "":
		rr.serveRemoteSimple(w, r, "")
	case strings.HasPrefix(p, "/simple/"):
		rr.serveRemoteSimple(w, r, strings.TrimPrefix(p, "/simple/"))
	case strings.HasPrefix(p, "/_remote/"):
		rr.serveRemoteFile(w, r, strings.TrimPrefix(p, "/_remote/"))
	case strings.HasPrefix(p, "/pypi/") && strings.HasSuffix(p, "/json"):
		rr.serveRemoteJSONAPI(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/pypi/"), "/json"))
	default:
		http.Error(w, "no pypi route", http.StatusNotFound)
	}
}

func cacheKey(name string) string {
	if name == "" {
		return "_root"
	}
	return normalizeName(name)
}

func (rr *Remote) cachedIndex(name string) *index {
	body, err := rr.store.readRaw(rr.store.simplePath(cacheKey(name)))
	if err != nil {
		return nil
	}
	idx, err := parseIndexJSON(body)
	if err != nil {
		return nil
	}
	return idx
}

// fetchIndex returns the upstream index page (absolute upstream URLs),
// preferring PEP 691 JSON and falling back to HTML parsing.
func (rr *Remote) fetchIndex(ctx context.Context, name string) (*index, error) {
	cachePath := rr.store.simplePath(cacheKey(name))
	if fi, err := rr.store.fs.Stat(cachePath); err == nil && (rr.mutableTTL <= 0 || time.Since(fi.ModTime) < rr.mutableTTL) {
		if idx := rr.cachedIndex(name); idx != nil {
			return idx, nil
		}
	}
	upath := "/simple/"
	if name != "" {
		upath += url.PathEscape(normalizeName(name)) + "/"
	}
	resp, err := rr.client.GetWithHeaders(ctx, upath, http.Header{"Accept": {acceptAll}})
	if err != nil {
		if !errors.Is(err, upstream.ErrNotFound) {
			if idx := rr.cachedIndex(name); idx != nil {
				return idx, nil
			}
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var idx *index
	if strings.Contains(strings.ToLower(resp.ContentType), "json") {
		idx, err = parseIndexJSON(body)
		if err != nil {
			return nil, err
		}
		if name != "" && idx.Name == "" {
			idx.Name = normalizeName(name)
		}
		base, _ := url.Parse(rr.client.BaseURL() + upath)
		for i := range idx.Files {
			if u, err := url.Parse(idx.Files[i].URL); err == nil && base != nil {
				idx.Files[i].URL = base.ResolveReference(u).String()
			}
		}
	} else {
		base, _ := url.Parse(rr.client.BaseURL() + upath)
		idx = parseIndexHTML(body, name, base)
	}
	if name == "" {
		idx.Name = ""
	}
	_ = rr.store.writeRaw(cachePath, idx.marshalJSON())
	return idx, nil
}

func (rr *Remote) serveRemoteSimple(w http.ResponseWriter, r *http.Request, name string) {
	idx, err := rr.fetchIndex(r.Context(), name)
	if err != nil {
		if errors.Is(err, upstream.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	out := *idx
	prefix := pkgbase.Prefix(r)
	out.LinkBase = prefix + "/simple/"
	out.Files = make([]indexFile, len(idx.Files))
	for i, f := range idx.Files {
		f.URL = prefix + "/_remote/" + encodeKey(f.URL) + "/" + url.PathEscape(f.Filename)
		out.Files[i] = f
	}
	writeIndex(w, r, &out)
}

func encodeKey(rawURL string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(rawURL))
}

// splitRemoteKey parses "{key}[/{file}][.metadata]".
func splitRemoteKey(rest string) (key, file string, meta bool) {
	key, file, _ = strings.Cut(rest, "/")
	if file != "" {
		file, meta = strings.CutSuffix(file, metadataSuffix)
	} else {
		key, meta = strings.CutSuffix(key, metadataSuffix)
	}
	return key, file, meta
}

// fetchRemote returns the cached upstream file (or its .metadata),
// fetching it on first access.
func (rr *Remote) fetchRemote(ctx context.Context, key string, meta bool) ([]byte, error) {
	cacheKey := key
	if meta {
		cacheKey += metadataSuffix
	}
	if body, err := rr.store.readRaw(rr.store.remotePath(cacheKey)); err == nil {
		return body, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("bad remote file key: %w", err)
	}
	target := string(raw)
	if meta {
		target += metadataSuffix
	}
	resp, err := rr.client.Get(ctx, target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = rr.store.WriteRemote(cacheKey, body)
	return body, nil
}

func (rr *Remote) serveRemoteFile(w http.ResponseWriter, r *http.Request, rest string) {
	key, file, meta := splitRemoteKey(rest)
	name := file
	if name == "" {
		name = key
	}
	if meta {
		name = metadataSuffix
	}
	body, err := rr.fetchRemote(r.Context(), key, meta)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "bad remote file key"):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, upstream.ErrNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		}
		return
	}
	if meta {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	writeFile(w, r, name, bytes.NewReader(body), &rawfs.FileInfo{Size: int64(len(body))})
}

func (rr *Remote) fetchJSONAPI(ctx context.Context, name, version string) ([]byte, error) {
	upath := "/pypi/" + url.PathEscape(name) + "/json"
	cachePath := rr.store.join("pypi-json", name+".json")
	if version != "" {
		upath = "/pypi/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/json"
		cachePath = rr.store.join("pypi-json", name, version+".json")
	}
	if fi, err := rr.store.fs.Stat(cachePath); err == nil && (rr.mutableTTL <= 0 || time.Since(fi.ModTime) < rr.mutableTTL) {
		if body, err := rr.store.readRaw(cachePath); err == nil {
			return body, nil
		}
	}
	resp, err := rr.client.GetWithHeaders(ctx, upath, http.Header{"Accept": {"application/json"}})
	if err != nil {
		if !errors.Is(err, upstream.ErrNotFound) {
			if body, rerr := rr.store.readRaw(cachePath); rerr == nil {
				return body, nil
			}
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = rr.store.writeRaw(cachePath, body)
	return body, nil
}

func (rr *Remote) serveRemoteJSONAPI(w http.ResponseWriter, r *http.Request, rest string) {
	name, version, _ := strings.Cut(rest, "/")
	body, err := rr.fetchJSONAPI(r.Context(), normalizeName(name), version)
	if err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	out, err := rewriteJSONAPI(body, pkgbase.PublicBase(r))
	if err != nil {
		pkgbase.Error(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", out)
}

// ── Virtual ──

// Virtual merges Simple API pages across members; every other request
// is served first-hit.
type Virtual struct{ *pkgbase.Virtual }

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		if len(r.Members) == 0 {
			return nil, fmt.Errorf("pypi/virtual %s/%s: members required", ns, r.Name)
		}
		v, err := pkgbase.NewVirtual(service.RegistryTypePyPI, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v}, nil
	}
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSuffix(r.URL.Path, "/")
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if read && (p == "" || p == "/simple" || strings.HasPrefix(p, "/simple/")) {
		v.serveMergedSimple(w, r, strings.TrimPrefix(strings.TrimPrefix(p, "/simple"), "/"))
		return
	}
	if !v.ServeFirstHit(w, r) {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (v *Virtual) serveMergedSimple(w http.ResponseWriter, r *http.Request, name string) {
	sub := r.Clone(r.Context())
	sub.Method = http.MethodGet
	sub.Header.Set("Accept", mediaJSON)
	q := sub.URL.Query()
	q.Del("format")
	sub.URL.RawQuery = q.Encode()
	var pages []*index
	for _, body := range v.CollectMembers(sub) {
		if idx, err := parseIndexJSON(body); err == nil {
			pages = append(pages, idx)
		}
	}
	if len(pages) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if name != "" {
		name = normalizeName(name)
	}
	merged := mergeIndexes(name, pages)
	merged.LinkBase = pkgbase.Prefix(r) + "/simple/"
	writeIndex(w, r, merged)
}

// ── shared ──

func packageDetail(s *Store, name string) (*registry.PackageDetail, error) {
	name = normalizeName(name)
	files, _ := s.ListPackageFiles(name)
	if len(files) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	pm, _ := s.LoadMeta(name)
	byVersion := map[string]*registry.PyPIVersionDetail{}
	for _, f := range files {
		v := fileVersion(pm, f.Name)
		if v == "" {
			v = "unknown"
		}
		row := byVersion[v]
		if row == nil {
			row = &registry.PyPIVersionDetail{Version: v}
			byVersion[v] = row
		}
		row.Files = append(row.Files, f.Name)
		row.FileSize += f.Size
	}
	return detailFrom(name, byVersion), nil
}

func detailFrom(name string, byVersion map[string]*registry.PyPIVersionDetail) *registry.PackageDetail {
	versions := make([]string, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	pkgbase.SortVersions(versions)
	detail := &registry.PyPIPackageDetail{}
	for _, v := range versions {
		detail.Versions = append(detail.Versions, *byVersion[v])
	}
	if len(versions) > 0 {
		detail.LatestVersion = versions[len(versions)-1]
	}
	return &registry.PackageDetail{Type: service.RegistryTypePyPI, Name: name, PyPI: detail}
}

func formatUploadTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

func writeBody(w http.ResponseWriter, r *http.Request, rc io.Reader, fi *rawfs.FileInfo) {
	if fi != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fi.Size))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
	}
}

func writeFile(w http.ResponseWriter, r *http.Request, filename string, rc io.Reader, fi *rawfs.FileInfo) {
	if w.Header().Get("Content-Type") == "" {
		if ct := mime.TypeByExtension(path.Ext(filename)); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}
	writeBody(w, r, rc, fi)
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("upload too large")
	}
	return body, nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "not found") || strings.Contains(low, "no such file") || strings.Contains(low, "does not exist")
}
