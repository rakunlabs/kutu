// Package generic implements a versioned raw-file registry:
//
//	PUT    /{package}/{version}/{filename}   upload (local, allow_push)
//	GET    /{package}/{version}/{filename}   download
//	GET    /{package}/{version}/             JSON file list
//	GET    /{package}/                       JSON version list
//	GET    /                                 JSON package list
//	DELETE /{package}/{version}/{filename}   delete one file
//	DELETE /{package}/{version}              delete a version
//
// Package names may contain "/" except as the final two segments,
// which are always {version}/{filename}. Remote repos proxy the same
// layout from an upstream (another kutu generic repo or any static
// file server); files are cached forever, listings honour MutableTTL.
package generic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeGeneric

// filesDir is the root under the repo base path holding artifacts.
const filesDir = "files"

// Store wraps pkgbase.Store with the generic layout.
type Store struct{ *pkgbase.Store }

func sortSummaries(s []registry.PackageSummary) {
	sort.Slice(s, func(i, j int) bool { return s[i].Name < s[j].Name })
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "\\\x00")
}

// ParsePath splits "/{pkg...}/{version}/{file}" (file may be empty).
func ParsePath(p string) (pkg, version, file string, ok bool) {
	p = strings.Trim(p, "/")
	parts := strings.Split(p, "/")
	if len(parts) < 3 {
		return "", "", "", false
	}
	file = parts[len(parts)-1]
	version = parts[len(parts)-2]
	pkg = strings.Join(parts[:len(parts)-2], "/")
	for _, s := range parts {
		if !validSegment(s) {
			return "", "", "", false
		}
	}
	return pkg, version, file, true
}

func (s *Store) rel(parts ...string) string {
	return path.Join(append([]string{filesDir}, parts...)...)
}

// ListPackages returns every package (dirs holding version dirs that
// hold files).
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	pkgs := map[string]map[string]struct{}{}
	err := s.Walk(filesDir, func(rel string, _ rawfs.DirEntry) error {
		inner := strings.TrimPrefix(rel, filesDir+"/")
		pkg, ver, _, ok := ParsePath(inner)
		if !ok {
			return nil
		}
		if pkgs[pkg] == nil {
			pkgs[pkg] = map[string]struct{}{}
		}
		pkgs[pkg][ver] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(pkgs))
	for name, set := range pkgs {
		vs := make([]string, 0, len(set))
		for v := range set {
			vs = append(vs, v)
		}
		pkgbase.SortVersions(vs)
		out = append(out, registry.PackageSummary{Name: name, Versions: vs})
	}
	sortSummaries(out)
	return out, nil
}

// ListVersions returns the versions of pkg (ascending).
func (s *Store) ListVersions(pkg string) ([]string, error) {
	dirs, err := s.ListDirs(s.rel(pkg))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		files, _ := s.ListFiles(s.rel(pkg, d))
		if len(files) > 0 {
			out = append(out, d)
		}
	}
	pkgbase.SortVersions(out)
	return out, nil
}

// ListFilesOf returns the files of pkg@version.
func (s *Store) ListFilesOf(pkg, version string) ([]registry.GenericFile, error) {
	files, err := s.ListFiles(s.rel(pkg, version))
	if err != nil {
		return nil, err
	}
	out := make([]registry.GenericFile, 0, len(files))
	for _, f := range files {
		out = append(out, registry.GenericFile{Name: f.Name, Size: f.Size})
	}
	return out, nil
}

// DeleteVersion removes every file of pkg@version.
func (s *Store) DeleteVersion(_ context.Context, pkg, version string) error {
	files, err := s.ListFiles(s.rel(pkg, version))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, f := range files {
		if err := s.Delete(s.rel(pkg, version, f.Name)); err != nil {
			return err
		}
	}
	return nil
}

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	name = strings.Trim(name, "/")
	versions, err := s.ListVersions(name)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d := &registry.GenericPackageDetail{LatestVersion: pkgbase.Latest(versions)}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		files, _ := s.ListFilesOf(name, v)
		row := registry.GenericVersionDetail{Version: v, Files: files}
		var newest time.Time
		for _, f := range files {
			row.Size += f.Size
			if fi, err := s.Stat(s.rel(name, v, f.Name)); err == nil && fi.ModTime.After(newest) {
				newest = fi.ModTime
			}
		}
		if !newest.IsZero() {
			row.PublishedAt = newest.UTC().Format(time.RFC3339)
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

// ── Local ──

type Local struct {
	*pkgbase.Repo
	store *Store
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

func (l *Local) DeleteVersion(ctx context.Context, name, version string) error {
	return l.store.DeleteVersion(ctx, name, version)
}

// PromoteVersion copies every file of name@version into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("generic: promote target must be a local generic repository")
	}
	name = strings.Trim(name, "/")
	files, err := l.store.ListFiles(l.store.rel(name, version))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return registry.ErrPackageNotFound
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	exists := false
	if dfs, _ := d.store.ListFiles(d.store.rel(name, version)); len(dfs) > 0 {
		exists = true
	}
	if code, err := d.Guard.Check(d.store.Store, exists, total); err != nil {
		return fmt.Errorf("promote rejected by target policy (%d): %w", code, err)
	}
	for _, f := range files {
		rc, fi, err := l.store.Open(l.store.rel(name, version, f.Name))
		if err != nil {
			return err
		}
		err = d.store.WriteStream(d.store.rel(name, version, f.Name), rc, fi.Size)
		rc.Close()
		if err != nil {
			return err
		}
	}
	d.EmitPublished(name+"@"+version, total)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveRead(w, r, l.store)
	case http.MethodPut, http.MethodPost:
		l.upload(w, r)
	case http.MethodDelete:
		l.remove(w, r, p)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	pkg, ver, file, ok := ParsePath(r.URL.Path)
	if !ok || file == "" {
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /{package}/{version}/{filename}")
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	rel := l.store.rel(pkg, ver, file)
	if !l.AllowPublish(w, l.store.Exists(rel), int64(len(body))) {
		return
	}
	if err := l.store.Write(rel, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(pkg+"@"+ver+"/"+file, int64(len(body)))
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"package": pkg, "version": ver, "file": file, "size": len(body), "sha256": pkgbase.SHA256Hex(body),
	})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request, p string) {
	if !l.CheckPush(w) {
		return
	}
	if pkg, ver, file, ok := ParsePath(p); ok && l.store.Exists(l.store.rel(pkg, ver, file)) {
		if err := l.store.Delete(l.store.rel(pkg, ver, file)); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		l.EmitDeleted(pkg + "@" + ver + "/" + file)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// DELETE /{pkg}/{version}
	trimmed := strings.Trim(p, "/")
	i := strings.LastIndex(trimmed, "/")
	if i <= 0 {
		pkgbase.NotFound(w)
		return
	}
	pkg, ver := trimmed[:i], trimmed[i+1:]
	if err := l.store.DeleteVersion(r.Context(), pkg, ver); err != nil {
		if pkgbase.IsNotFound(err) {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitDeleted(pkg + "@" + ver)
	w.WriteHeader(http.StatusNoContent)
}

// serveRead handles file downloads and the JSON listings.
func serveRead(w http.ResponseWriter, r *http.Request, s *Store) {
	trimmed := strings.Trim(r.URL.Path, "/")
	if trimmed == "" {
		pkgs, err := s.ListPackages(r.Context())
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"packages": pkgs})
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/") {
		if pkg, ver, file, ok := ParsePath(trimmed); ok {
			if pkgbase.ServeStored(w, r, s.Store, s.rel(pkg, ver, file), "application/octet-stream") {
				return
			}
		}
	}
	// "{pkg}/{version}/" → file list, "{pkg}/" → version list.
	if i := strings.LastIndex(trimmed, "/"); i > 0 {
		pkg, ver := trimmed[:i], trimmed[i+1:]
		if files, _ := s.ListFilesOf(pkg, ver); len(files) > 0 {
			pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"package": pkg, "version": ver, "files": files})
			return
		}
	}
	if versions, _ := s.ListVersions(trimmed); len(versions) > 0 {
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"package": trimmed, "versions": versions, "latest": pkgbase.Latest(versions)})
		return
	}
	pkgbase.NotFound(w)
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	if strings.HasSuffix(r.URL.Path, "/") {
		return registry.ArtifactRef{}, false
	}
	pkg, ver, _, ok := ParsePath(r.URL.Path)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: pkg, Version: ver}, true
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func (rr *Remote) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return rr.store.ListPackages(ctx)
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return detail(rr.store, name)
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	// Generic files are immutable; only a deep purge removes anything.
	if !opts.All {
		return registry.PurgeStats{}, nil
	}
	return rr.Purge(opts), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

// Prefetch warms the version listing and, for a pinned (or latest)
// version, every file of that version.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	name = strings.Trim(name, "/")
	if version == "" {
		body, err := rr.FetchCached(ctx, path.Join("meta", name, "_index.json"), "/"+name+"/", true)
		if err != nil {
			return err
		}
		var listing struct {
			Latest   string   `json:"latest"`
			Versions []string `json:"versions"`
		}
		if json.Unmarshal(body, &listing) != nil {
			return nil
		}
		version = listing.Latest
		if version == "" {
			version = pkgbase.Latest(listing.Versions)
		}
		if version == "" {
			return nil
		}
	}
	body, err := rr.FetchCached(ctx, path.Join("meta", name, version, "_index.json"), "/"+name+"/"+version+"/", true)
	if err != nil {
		return err
	}
	var files struct {
		Files []registry.GenericFile `json:"files"`
	}
	if err := json.Unmarshal(body, &files); err != nil {
		return err
	}
	for _, f := range files.Files {
		if _, err := rr.FetchCached(ctx, rr.store.rel(name, version, f.Name), "/"+name+"/"+version+"/"+f.Name, false); err != nil {
			return err
		}
	}
	return nil
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	trimmed := strings.Trim(r.URL.Path, "/")
	if pkg, ver, file, ok := ParsePath(trimmed); ok && !strings.HasSuffix(r.URL.Path, "/") {
		rel := rr.store.rel(pkg, ver, file)
		if pkgbase.ServeStored(w, r, rr.store.Store, rel, "application/octet-stream") {
			return
		}
		body, err := rr.FetchCached(r.Context(), rel, "/"+trimmed, false)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/octet-stream", body)
		return
	}
	// Listings are proxied (and cached under meta/) so they reflect
	// the upstream view rather than only what has been pulled so far.
	cacheRel := path.Join("meta", trimmed, "_index.json")
	upath := "/" + trimmed
	if strings.HasSuffix(r.URL.Path, "/") || trimmed == "" {
		upath += "/"
	}
	rr.ServeCached(w, r, cacheRel, upath, "application/json", true)
}

// ── Virtual ──

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return pkgbase.NewVirtualFactory(typ, resolver)
}
