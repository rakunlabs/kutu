// Package composer implements a Composer v2 (Packagist-compatible)
// repository:
//
//	GET    /packages.json                         repository root (metadata-url, search, providers-api, list)
//	GET    /p2/{vendor}/{name}.json               stable versions (non-minified Composer 2 metadata)
//	GET    /p2/{vendor}/{name}~dev.json           dev-* / *-dev versions
//	GET    /dist/{vendor}/{name}/{version}.zip    dist archive
//	GET    /search.json?q=&type=                  {results:[{name,description,url}],total}
//	GET    /list.json                             {packageNames:[...]}
//	GET    /providers/{vendor}/{name}.json        packages that provide/replace a name
//	PUT    /api/upload[?version=V]                publish (local; body = package zip)
//	POST   /api/upload[?version=V]                publish (local; raw zip or multipart field "file", optional field "version")
//	DELETE /api/packages/{vendor}/{name}/{version} delete a version (local)
//
// The uploaded zip must contain composer.json at its root or inside a
// single top-level directory. The package name comes from
// composer.json; the version from the "version" query/form parameter
// or, failing that, composer.json "version".
//
// Remote repos proxy a Composer v2 upstream (e.g.
// https://repo.packagist.org): p2 metadata is cached with MutableTTL
// and every dist.url is rewritten to /dist/{vendor}/{name}/{version}.zip
// on kutu; the original URL (often an absolute GitHub zipball URL) is
// looked up from the cached metadata on download and the archive is
// cached forever. Virtual repos merge p2 version lists across members
// (first member wins per version).
//
// Client configuration:
//
//	composer.json: "repositories": [{"type": "composer", "url": "https://kutu.example.com/registries/{ns}/{repo}"}]
//	composer config --global http-basic.kutu.example.com x <token>
//	curl -u x:<token> -T pkg.zip "https://kutu.example.com/registries/{ns}/{repo}/api/upload?version=1.0.0"
//
// Packagist's "packagist.org" entry can be disabled with
// "repositories": [{"packagist.org": false}, ...] to force all
// resolution through kutu.
package composer

import (
	"context"
	"crypto/sha1" //nolint:gosec // Composer dist shasum is SHA-1.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeComposer

const (
	pkgDir   = "packages"
	indexDir = "index"
	metaDir  = "meta"
	distDir  = "dist"
)

// Store wraps pkgbase.Store with the composer layout.
type Store struct{ *pkgbase.Store }

func (s *Store) zipRel(name, version string) string {
	return path.Join(pkgDir, name, escVersion(version)+".zip")
}

func (s *Store) jsonRel(name, version string) string {
	return path.Join(pkgDir, name, escVersion(version)+".json")
}

func (s *Store) indexRel(name string) string { return path.Join(indexDir, name+".json") }

type indexDoc struct {
	Name     string           `json:"name"`
	Versions []map[string]any `json:"versions"`
}

// Versions returns every stored version object of name (newest first).
func (s *Store) Versions(name string) ([]map[string]any, error) {
	body, err := s.Read(s.indexRel(name))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, registry.ErrPackageNotFound
		}
		return nil, err
	}
	var doc indexDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if len(doc.Versions) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	return doc.Versions, nil
}

// Version returns one stored version object.
func (s *Store) Version(name, version string) (map[string]any, error) {
	body, err := s.Read(s.jsonRel(name, version))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, registry.ErrPackageNotFound
		}
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(body, &m)
}

// RebuildIndex regenerates the per-package index from the version
// files, removing it when no version remains.
func (s *Store) RebuildIndex(name string) error {
	files, err := s.ListFiles(path.Join(pkgDir, name))
	if err != nil {
		return err
	}
	var vs []map[string]any
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		body, err := s.Read(path.Join(pkgDir, name, f.Name))
		if err != nil {
			return err
		}
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			vs = append(vs, m)
		}
	}
	if len(vs) == 0 {
		return s.Delete(s.indexRel(name))
	}
	sortVersionsDesc(vs)
	body, err := pkgbase.MarshalJSON(indexDoc{Name: name, Versions: vs})
	if err != nil {
		return err
	}
	return s.Write(s.indexRel(name), body)
}

// Names returns every package with an index (sorted).
func (s *Store) Names() ([]string, error) {
	vendors, err := s.ListDirs(indexDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vendors {
		files, err := s.ListFiles(path.Join(indexDir, v))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if n, ok := strings.CutSuffix(f.Name, ".json"); ok {
				out = append(out, v+"/"+n)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListPackages implements registry.PackageLister.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := s.Names()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(names))
	for _, n := range names {
		vs, err := s.Versions(n)
		if err != nil {
			continue
		}
		row := registry.PackageSummary{Name: n}
		for _, v := range vs {
			row.Versions = append(row.Versions, str(v, "version"))
		}
		pkgbase.SortVersions(row.Versions)
		out = append(out, row)
	}
	return out, nil
}

// Put stores a version (zip + metadata) and regenerates the index.
func (s *Store) Put(name, version string, obj map[string]any, zipBody []byte) error {
	meta, err := pkgbase.MarshalJSON(obj)
	if err != nil {
		return err
	}
	if err := s.Write(s.zipRel(name, version), zipBody); err != nil {
		return err
	}
	if err := s.Write(s.jsonRel(name, version), meta); err != nil {
		return err
	}
	return s.RebuildIndex(name)
}

// DeleteVersion removes name@version and regenerates the index.
func (s *Store) DeleteVersion(_ context.Context, name, version string) error {
	name = strings.ToLower(strings.Trim(name, "/"))
	if !s.Exists(s.jsonRel(name, version)) && !s.Exists(s.zipRel(name, version)) {
		return registry.ErrPackageNotFound
	}
	if err := s.Delete(s.zipRel(name, version)); err != nil {
		return err
	}
	if err := s.Delete(s.jsonRel(name, version)); err != nil {
		return err
	}
	return s.RebuildIndex(name)
}

// render returns client-facing copies of vs with dist.url pointing at base.
func render(base, name string, vs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		c := cloneMap(v)
		dist := map[string]any{"type": "zip"}
		if d, ok := v["dist"].(map[string]any); ok {
			dist = cloneMap(d)
		}
		dist["url"] = base + distPath(name, str(v, "version"))
		c["dist"] = dist
		out = append(out, c)
	}
	return out
}

func splitByStability(vs []map[string]any, dev bool) []map[string]any {
	var out []map[string]any
	for _, v := range vs {
		if IsDevVersion(str(v, "version")) == dev {
			out = append(out, v)
		}
	}
	return out
}

func writeP2(w http.ResponseWriter, r *http.Request, name string, vs []map[string]any) {
	if vs == nil {
		vs = []map[string]any{}
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"packages": map[string]any{name: vs}})
}

func writeSearch(w http.ResponseWriter, r *http.Request, results []searchResult) {
	if results == nil {
		results = []searchResult{}
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"results": results, "total": len(results)})
}

func latestObj(vs []map[string]any) map[string]any {
	var best map[string]any
	for _, v := range vs {
		if IsDevVersion(str(v, "version")) {
			continue
		}
		if best == nil || pkgbase.CompareVersions(str(v, "version"), str(best, "version")) > 0 {
			best = v
		}
	}
	if best == nil && len(vs) > 0 {
		best = vs[0]
	}
	return best
}

// packageSet abstracts "all known packages" for search / list / providers.
type packageSet func() map[string][]map[string]any

func serveSearch(w http.ResponseWriter, r *http.Request, set packageSet) {
	q := r.URL.Query().Get("q")
	tf := r.URL.Query().Get("type")
	base := pkgbase.PublicBase(r)
	all := set()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	var res []searchResult
	for _, n := range names {
		v := latestObj(all[n])
		if v == nil || !matchSearch(q, tf, v) {
			continue
		}
		res = append(res, searchResult{Name: n, Description: str(v, "description"), URL: base + "/p2/" + n + ".json"})
	}
	writeSearch(w, r, res)
}

func serveProviders(w http.ResponseWriter, r *http.Request, target string, set packageSet) {
	all := set()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	providers := []map[string]any{}
	for _, n := range names {
		for _, v := range all[n] {
			if n == target || provides(v, target) {
				providers = append(providers, map[string]any{"name": n, "description": str(v, "description"), "type": str(v, "type")})
				break
			}
		}
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"providers": providers})
}

func (s *Store) all() map[string][]map[string]any {
	out := map[string][]map[string]any{}
	names, _ := s.Names()
	for _, n := range names {
		if vs, err := s.Versions(n); err == nil {
			out[n] = vs
		}
	}
	return out
}

func (s *Store) detail(name string) (*registry.PackageDetail, error) {
	name = strings.ToLower(strings.Trim(name, "/"))
	vs, err := s.Versions(name)
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	for _, v := range vs {
		ver := str(v, "version")
		if fi, err := s.Stat(s.zipRel(name, ver)); err == nil {
			sizes[ver] = fi.Size
		}
	}
	return detailFrom(name, vs, sizes), nil
}

func artifactMeta(v map[string]any) registry.ArtifactMeta {
	return registry.ArtifactMeta{License: licenseOf(v), PublishedAt: timeOf(v)}
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b) //nolint:gosec
	return hex.EncodeToString(sum[:])
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
	return l.store.detail(name)
}

func (l *Local) DeleteVersion(ctx context.Context, name, version string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.store.DeleteVersion(ctx, name, version); err != nil {
		return err
	}
	l.EmitDeleted(strings.ToLower(name) + "@" + version)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r.URL.Path)
}

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	v, err := l.store.Version(strings.ToLower(ref.Name), ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	return artifactMeta(v), nil
}

// PromoteVersion copies name@version into dst (a composer Local).
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("composer promote: destination is not a local composer repository")
	}
	name = strings.ToLower(name)
	obj, err := l.store.Version(name, version)
	if err != nil {
		return err
	}
	body, err := l.store.Read(l.store.zipRel(name, version))
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.Guard.Check(d.store.Store, d.store.Exists(d.store.zipRel(name, version)), int64(len(body))); err != nil {
		return err
	}
	if err := d.store.Put(name, version, obj, body); err != nil {
		return err
	}
	d.EmitPublished(name+"@"+version, int64(len(body)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		l.serveRead(w, r, p)
	case http.MethodPut, http.MethodPost:
		if p == "/api/upload" || p == "/api/upload/" {
			l.upload(w, r)
			return
		}
		pkgbase.NotFound(w)
	case http.MethodDelete:
		l.remove(w, r, p)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) serveRead(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/" || p == "" || p == "/packages.json":
		names, err := l.store.Names()
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if names == nil {
			names = []string{}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, rootDoc(pkgbase.Prefix(r), names))
	case p == "/list.json":
		names, _ := l.store.Names()
		if names == nil {
			names = []string{}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"packageNames": names})
	case p == "/search.json":
		serveSearch(w, r, l.store.all)
	case strings.HasPrefix(p, "/providers/"):
		name, ok := splitProviders(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		serveProviders(w, r, name, l.store.all)
	case strings.HasPrefix(p, "/p2/"):
		name, dev, ok := splitP2(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		vs, err := l.store.Versions(name)
		if err != nil {
			pkgbase.NotFound(w)
			return
		}
		writeP2(w, r, name, render(pkgbase.PublicBase(r), name, splitByStability(vs, dev)))
	case strings.HasPrefix(p, "/dist/"):
		name, ver, ok := splitDist(p)
		if !ok || !pkgbase.ServeStored(w, r, l.store.Store, l.store.zipRel(name, ver), "application/zip") {
			pkgbase.NotFound(w)
		}
	default:
		pkgbase.NotFound(w)
	}
}

func (l *Local) readUpload(r *http.Request) ([]byte, string, error) {
	version := r.URL.Query().Get("version")
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		max := l.MaxUpload
		if max <= 0 {
			max = pkgbase.DefaultMaxUpload
		}
		r.Body = http.MaxBytesReader(nil, r.Body, max+1<<20)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, "", fmt.Errorf("parse multipart: %w", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			return nil, "", errors.New(`missing multipart field "file"`)
		}
		defer f.Close()
		body, err := pkgbase.ReadBody(f, l.MaxUpload)
		if err != nil {
			return nil, "", err
		}
		if v := r.FormValue("version"); v != "" {
			version = v
		}
		return body, version, nil
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	return body, version, err
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	body, version, err := l.readUpload(r)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	cj, err := ReadComposerJSON(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.ToLower(str(cj, "name"))
	if !ValidName(name) {
		pkgbase.Error(w, http.StatusBadRequest, fmt.Sprintf("composer.json: invalid or missing package name %q", str(cj, "name")))
		return
	}
	if version == "" {
		version = str(cj, "version")
	}
	if version == "" {
		pkgbase.Error(w, http.StatusBadRequest, `version required: set "version" in composer.json or pass ?version=`)
		return
	}
	if !validVersion(version) {
		pkgbase.Error(w, http.StatusBadRequest, "invalid version")
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.AllowPublish(w, l.store.Exists(l.store.zipRel(name, version)), int64(len(body))) {
		return
	}
	sum := sha1Hex(body)
	obj := buildVersion(cj, name, version, time.Now())
	obj["dist"] = map[string]any{"type": "zip", "shasum": sum, "reference": sum}
	if err := l.store.Put(name, version, obj, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(name+"@"+version, int64(len(body)))
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"name": name, "version": version, "shasum": sum, "size": len(body),
		"url": pkgbase.PublicBase(r) + distPath(name, version),
	})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request, p string) {
	if !l.CheckPush(w) {
		return
	}
	rest, ok := strings.CutPrefix(p, "/api/packages/")
	parts := strings.SplitN(rest, "/", 3)
	if !ok || len(parts) != 3 {
		pkgbase.Error(w, http.StatusBadRequest, "expected DELETE /api/packages/{vendor}/{name}/{version}")
		return
	}
	name := strings.ToLower(parts[0] + "/" + parts[1])
	if err := l.DeleteVersion(r.Context(), name, unescVersion(parts[2])); err != nil {
		if pkgbase.IsNotFound(err) {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/packages.json")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func p2Cache(name string, dev bool) string {
	if dev {
		return path.Join(metaDir, "p2", name+"~dev.json")
	}
	return path.Join(metaDir, "p2", name+".json")
}

// upstreamVersions fetches (or reads cached) upstream p2 metadata.
func (rr *Remote) upstreamVersions(ctx context.Context, name string, dev bool) ([]map[string]any, error) {
	up := "/p2/" + name + ".json"
	if dev {
		up = "/p2/" + name + "~dev.json"
	}
	body, err := rr.FetchCached(ctx, p2Cache(name, dev), up, true)
	if err != nil {
		return nil, err
	}
	return versionsFromBody(body, name)
}

func versionsFromBody(body []byte, name string) ([]map[string]any, error) {
	doc, err := parseP2(body)
	if err != nil {
		return nil, err
	}
	return doc[name], nil
}

// cachedVersions returns cached stable + dev versions without network.
func (rr *Remote) cachedVersions(name string) []map[string]any {
	var out []map[string]any
	for _, dev := range []bool{false, true} {
		if body, err := rr.store.Read(p2Cache(name, dev)); err == nil {
			if vs, err := versionsFromBody(body, name); err == nil {
				out = append(out, vs...)
			}
		}
	}
	return out
}

func (rr *Remote) cachedNames() []string {
	var out []string
	seen := map[string]bool{}
	_ = rr.store.Walk(path.Join(metaDir, "p2"), func(rel string, _ rawfs.DirEntry) error {
		n := strings.TrimPrefix(rel, metaDir+"/p2/")
		n = strings.TrimSuffix(strings.TrimSuffix(n, ".json"), "~dev")
		if ValidName(n) && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (rr *Remote) all() map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, n := range rr.cachedNames() {
		if vs := rr.cachedVersions(n); len(vs) > 0 {
			out[n] = vs
		}
	}
	return out
}

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	var out []registry.PackageSummary
	for _, n := range rr.cachedNames() {
		row := registry.PackageSummary{Name: n}
		for _, v := range rr.cachedVersions(n) {
			row.Versions = append(row.Versions, str(v, "version"))
		}
		pkgbase.SortVersions(row.Versions)
		out = append(out, row)
	}
	return out, nil
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	name = strings.ToLower(strings.Trim(name, "/"))
	vs := rr.cachedVersions(name)
	if len(vs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	sizes := map[string]int64{}
	for _, v := range vs {
		ver := str(v, "version")
		if fi, err := rr.store.Stat(rr.distCache(name, v)); err == nil {
			sizes[ver] = fi.Size
		}
	}
	return detailFrom(name, vs, sizes), nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, metaDir), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r.URL.Path)
}

func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	name := strings.ToLower(ref.Name)
	vs := rr.cachedVersions(name)
	if len(vs) == 0 {
		var err error
		if vs, err = rr.upstreamVersions(ctx, name, IsDevVersion(ref.Version)); err != nil {
			return registry.ArtifactMeta{}, registry.ErrPackageNotFound
		}
	}
	for _, v := range vs {
		if str(v, "version") == ref.Version {
			return artifactMeta(v), nil
		}
	}
	return registry.ArtifactMeta{}, registry.ErrPackageNotFound
}

// Prefetch warms p2 metadata and the dist of version (or latest stable).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	name = strings.ToLower(strings.Trim(name, "/"))
	vs, err := rr.upstreamVersions(ctx, name, version != "" && IsDevVersion(version))
	if err != nil {
		return err
	}
	var target map[string]any
	if version == "" {
		target = latestObj(vs)
	} else {
		for _, v := range vs {
			if str(v, "version") == version {
				target = v
				break
			}
		}
	}
	if target == nil {
		return registry.ErrPackageNotFound
	}
	_, err = rr.fetchDist(ctx, name, target)
	return err
}

// distCache is the cache path for the dist of version object v. Dev
// versions are keyed by dist reference since their content moves.
func (rr *Remote) distCache(name string, v map[string]any) string {
	ver := str(v, "version")
	if !IsDevVersion(ver) {
		return path.Join(distDir, name, escVersion(ver)+".zip")
	}
	ref := "none"
	if d, ok := v["dist"].(map[string]any); ok && str(d, "reference") != "" {
		sum := sha256.Sum256([]byte(str(d, "reference")))
		ref = hex.EncodeToString(sum[:16])
	}
	return path.Join(distDir, name, escVersion(ver), ref+".zip")
}

func (rr *Remote) originURL(v map[string]any) (string, error) {
	d, ok := v["dist"].(map[string]any)
	if !ok || str(d, "url") == "" {
		return "", fmt.Errorf("%w: version has no dist", registry.ErrPackageNotFound)
	}
	u, err := url.Parse(str(d, "url"))
	if err != nil {
		return "", err
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	base, err := url.Parse(rr.Client.BaseURL() + "/")
	if err != nil {
		return "", err
	}
	return base.ResolveReference(u).String(), nil
}

func (rr *Remote) fetchDist(ctx context.Context, name string, v map[string]any) ([]byte, error) {
	origin, err := rr.originURL(v)
	if err != nil {
		return nil, err
	}
	return rr.FetchCached(ctx, rr.distCache(name, v), origin, false)
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/" || p == "" || p == "/packages.json":
		pkgbase.WriteJSON(w, r, http.StatusOK, rootDoc(pkgbase.Prefix(r), nil))
	case p == "/list.json":
		names := rr.cachedNames()
		if names == nil {
			names = []string{}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"packageNames": names})
	case p == "/search.json":
		rr.serveSearch(w, r)
	case strings.HasPrefix(p, "/providers/"):
		name, ok := splitProviders(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		serveProviders(w, r, name, rr.all)
	case strings.HasPrefix(p, "/p2/"):
		name, dev, ok := splitP2(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		vs, err := rr.upstreamVersions(r.Context(), name, dev)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		writeP2(w, r, name, render(pkgbase.PublicBase(r), name, vs))
	case strings.HasPrefix(p, "/dist/"):
		rr.serveDist(w, r, p)
	default:
		pkgbase.NotFound(w)
	}
}

func (rr *Remote) serveDist(w http.ResponseWriter, r *http.Request, p string) {
	name, ver, ok := splitDist(p)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	if !IsDevVersion(ver) && pkgbase.ServeStored(w, r, rr.store.Store, path.Join(distDir, name, escVersion(ver)+".zip"), "application/zip") {
		return
	}
	vs, err := rr.upstreamVersions(r.Context(), name, IsDevVersion(ver))
	if err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	for _, v := range vs {
		if str(v, "version") != ver {
			continue
		}
		if pkgbase.ServeStored(w, r, rr.store.Store, rr.distCache(name, v), "application/zip") {
			return
		}
		body, err := rr.fetchDist(r.Context(), name, v)
		if err != nil {
			if errors.Is(err, registry.ErrPackageNotFound) {
				pkgbase.NotFound(w)
				return
			}
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/zip", body)
		return
	}
	pkgbase.NotFound(w)
}

// serveSearch proxies the upstream's advertised search endpoint and
// falls back to searching cached metadata.
func (rr *Remote) serveSearch(w http.ResponseWriter, r *http.Request) {
	if res, err := rr.upstreamSearch(r); err == nil {
		base := pkgbase.PublicBase(r)
		for i := range res {
			res[i].URL = base + "/p2/" + res[i].Name + ".json"
		}
		writeSearch(w, r, res)
		return
	}
	serveSearch(w, r, rr.all)
}

func (rr *Remote) upstreamSearch(r *http.Request) ([]searchResult, error) {
	ctx := r.Context()
	body, err := rr.FetchCached(ctx, path.Join(metaDir, "packages.json"), "/packages.json", true)
	if err != nil {
		return nil, err
	}
	var root struct {
		Search string `json:"search"`
	}
	if err := json.Unmarshal(body, &root); err != nil || root.Search == "" {
		return nil, errors.New("upstream has no search endpoint")
	}
	q := r.URL.Query()
	tmpl := strings.ReplaceAll(root.Search, "%query%", url.QueryEscape(q.Get("q")))
	tmpl = strings.ReplaceAll(tmpl, "%type%", url.QueryEscape(q.Get("type")))
	u, err := url.Parse(tmpl)
	if err != nil {
		return nil, err
	}
	if !u.IsAbs() {
		base, _ := url.Parse(rr.Client.BaseURL() + "/")
		u = base.ResolveReference(u)
	}
	resp, err := rr.Client.Get(ctx, u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Results []searchResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// ── Virtual ──

// Virtual merges p2 metadata, search and listings across members.
type Virtual struct{ *pkgbase.Virtual }

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := pkgbase.NewVirtual(typ, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v}, nil
	}
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/" || p == "" || p == "/packages.json":
		pkgbase.WriteJSON(w, r, http.StatusOK, rootDoc(pkgbase.Prefix(r), v.available(r.Context())))
	case p == "/list.json":
		seen := map[string]bool{}
		names := []string{}
		for _, body := range v.CollectMembers(r) {
			var doc struct {
				PackageNames []string `json:"packageNames"`
			}
			if json.Unmarshal(body, &doc) != nil {
				continue
			}
			for _, n := range doc.PackageNames {
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			}
		}
		sort.Strings(names)
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"packageNames": names})
	case p == "/search.json":
		seen := map[string]bool{}
		var res []searchResult
		for _, body := range v.CollectMembers(r) {
			var doc struct {
				Results []searchResult `json:"results"`
			}
			if json.Unmarshal(body, &doc) != nil {
				continue
			}
			for _, x := range doc.Results {
				if !seen[x.Name] {
					seen[x.Name] = true
					res = append(res, x)
				}
			}
		}
		writeSearch(w, r, res)
	case strings.HasPrefix(p, "/providers/"):
		seen := map[string]bool{}
		providers := []map[string]any{}
		for _, body := range v.CollectMembers(r) {
			var doc struct {
				Providers []map[string]any `json:"providers"`
			}
			if json.Unmarshal(body, &doc) != nil {
				continue
			}
			for _, x := range doc.Providers {
				if n := str(x, "name"); !seen[n] {
					seen[n] = true
					providers = append(providers, x)
				}
			}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"providers": providers})
	case strings.HasPrefix(p, "/p2/"):
		name, _, ok := splitP2(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		bodies := v.CollectMembers(r)
		if len(bodies) == 0 {
			pkgbase.NotFound(w)
			return
		}
		seen := map[string]bool{}
		merged := []map[string]any{}
		for _, body := range bodies {
			vs, err := versionsFromBody(body, name)
			if err != nil {
				continue
			}
			for _, x := range vs {
				if ver := str(x, "version"); !seen[ver] {
					seen[ver] = true
					merged = append(merged, x)
				}
			}
		}
		sortVersionsDesc(merged)
		writeP2(w, r, name, merged)
	default:
		if !v.ServeFirstHit(w, r) {
			pkgbase.NotFound(w)
		}
	}
}

// available returns the union of member package names when every
// member is local (remote members can serve arbitrary names).
func (v *Virtual) available(ctx context.Context) []string {
	allLocal := true
	v.ForEachMember(func(reg registry.Registry) bool {
		if reg.Kind() != service.RegistryKindLocal {
			allLocal = false
			return true
		}
		return false
	})
	if !allLocal {
		return nil
	}
	pkgs, err := v.ListPackages(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}
