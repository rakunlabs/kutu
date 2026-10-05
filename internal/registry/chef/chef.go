// Package chef implements a Chef Supermarket compatible cookbook
// registry (the subset used by Berkshelf and `knife supermarket`):
//
//	GET    /universe                                       dependency universe (Berkshelf)
//	GET    /api/v1/cookbooks?start=&items=                 paginated cookbook list
//	GET    /api/v1/search?q=&start=&items=                 cookbook search
//	GET    /api/v1/cookbooks/{name}                        cookbook document
//	GET    /api/v1/cookbooks/{name}/versions/{v}           version document ("1_2_3" accepted)
//	GET    /api/v1/cookbooks/{name}/versions/{v}/download  cookbook tarball
//	POST   /api/v1/cookbooks                               publish (multipart: tarball, cookbook)
//	PUT    /api/v1/cookbooks/{name}/{version}              publish a raw .tar.gz body
//	DELETE /api/v1/cookbooks/{name}/versions/{v}           delete a version (local)
//	DELETE /api/v1/cookbooks/{name}                        delete every version (local)
//
// The tarball must contain {dir}/metadata.json; when only
// metadata.rb is present its name/version/license/depends lines are
// parsed. `knife supermarket share` signs requests with Chef's
// mixlib-authentication which kutu cannot verify, so publish with a
// kutu token instead (curl or any HTTP client).
//
// Remote repos proxy a Supermarket (URL e.g. https://supermarket.chef.io):
// the universe, cookbook documents and listings honour MutableTTL and
// every upstream URL is rewritten to point at kutu; version documents
// and tarballs are cached forever.
//
// Client configuration:
//
//	# Berksfile
//	source "https://kutu.example.com/registries/{ns}/{repo}"
//
//	# knife.rb / config.rb (download, search, show)
//	knife[:supermarket_site] = "https://kutu.example.com/registries/{ns}/{repo}"
//
//	# publish
//	curl -X PUT -H "Authorization: Bearer <token>" --data-binary @apt-7.5.0.tar.gz \
//	  https://kutu.example.com/registries/{ns}/{repo}/api/v1/cookbooks/apt/7.5.0
package chef

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeChef

const (
	cbDir      = "cookbooks"
	metaDir    = "meta"
	recordFile = "version.json"
)

var (
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	versionRe  = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)
	rbStringRe = `\s*\(?\s*['"]([^'"]+)['"]`
	rbNameRe   = regexp.MustCompile(`(?m)^\s*name` + rbStringRe)
	rbVerRe    = regexp.MustCompile(`(?m)^\s*version` + rbStringRe)
	rbLicRe    = regexp.MustCompile(`(?m)^\s*license` + rbStringRe)
	rbDescRe   = regexp.MustCompile(`(?m)^\s*description` + rbStringRe)
	rbMaintRe  = regexp.MustCompile(`(?m)^\s*maintainer` + rbStringRe)
	rbSrcRe    = regexp.MustCompile(`(?m)^\s*source_url` + rbStringRe)
	rbIssRe    = regexp.MustCompile(`(?m)^\s*issues_url` + rbStringRe)
	rbDependRe = regexp.MustCompile(`(?m)^\s*depends\s*\(?\s*['"]([^'"]+)['"](?:\s*,\s*['"]([^'"]+)['"])?`)
	apiURLRe   = regexp.MustCompile(`^https?://[^/]+(?:/.*?)?/api/v1/(cookbooks/.*)$`)
)

// cookbookVersion is the stored metadata of one cookbook version.
type cookbookVersion struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	License      string            `json:"license,omitempty"`
	Description  string            `json:"description,omitempty"`
	Maintainer   string            `json:"maintainer,omitempty"`
	SourceURL    string            `json:"source_url,omitempty"`
	IssuesURL    string            `json:"issues_url,omitempty"`
	Category     string            `json:"category,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Platforms    map[string]string `json:"platforms,omitempty"`
	Size         int64             `json:"tarball_file_size,omitempty"`
	SHA256       string            `json:"sha256,omitempty"`
	PublishedAt  string            `json:"published_at,omitempty"`
	// UpstreamFile is the upstream download URL (remote only).
	UpstreamFile string `json:"upstream_file,omitempty"`
}

func validName(n string) bool { return nameRe.MatchString(n) && n != "." && n != ".." }

// normVersion accepts Supermarket's underscore-escaped versions.
func normVersion(v string) (string, bool) {
	v = strings.ReplaceAll(v, "_", ".")
	return v, versionRe.MatchString(v)
}

func tarballName(name, v string) string { return name + "-" + v + ".tar.gz" }

// ── routing ──

type route struct {
	kind    string // universe | list | search | cookbook | version | download | publish
	name    string
	version string
}

func parseRoute(method, p string) (route, bool) {
	p = strings.TrimSuffix(p, "/")
	switch p {
	case "/universe":
		return route{kind: "universe"}, true
	case "/api/v1/cookbooks":
		if method == http.MethodPost {
			return route{kind: "publish"}, true
		}
		return route{kind: "list"}, true
	case "/api/v1/search":
		return route{kind: "search"}, true
	}
	rest, ok := strings.CutPrefix(p, "/api/v1/cookbooks/")
	if !ok {
		return route{}, false
	}
	parts := strings.Split(rest, "/")
	if !validName(parts[0]) {
		return route{}, false
	}
	rt := route{name: parts[0]}
	switch {
	case len(parts) == 1:
		rt.kind = "cookbook"
	case len(parts) == 2 && method == http.MethodPut:
		rt.kind = "publish"
		if rt.version, ok = normVersion(parts[1]); !ok {
			return route{}, false
		}
	case len(parts) >= 3 && len(parts) <= 4 && parts[1] == "versions":
		v, ok := normVersion(parts[2])
		if !ok {
			return route{}, false
		}
		rt.version = v
		switch {
		case len(parts) == 3:
			rt.kind = "version"
		case parts[3] == "download":
			rt.kind = "download"
		default:
			return route{}, false
		}
	default:
		return route{}, false
	}
	return rt, true
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	rt, ok := parseRoute(r.Method, r.URL.Path)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	switch rt.kind {
	case "download":
		return registry.ArtifactRef{Name: rt.name, Version: rt.version}, true
	case "cookbook", "version":
		return registry.ArtifactRef{Name: rt.name}, true
	}
	return registry.ArtifactRef{}, false
}

// ── rendering ──

func apiBase(r *http.Request) string { return pkgbase.PublicBase(r) + "/api/v1" }

func cookbookURL(r *http.Request, name string) string { return apiBase(r) + "/cookbooks/" + name }

func versionURL(r *http.Request, name, v string) string {
	return cookbookURL(r, name) + "/versions/" + v
}

func downloadURL(r *http.Request, name, v string) string { return versionURL(r, name, v) + "/download" }

func universeEntry(r *http.Request, cv *cookbookVersion) map[string]any {
	deps := cv.Dependencies
	if deps == nil {
		deps = map[string]string{}
	}
	return map[string]any{
		"location_type": "opscode", "location_path": apiBase(r),
		"download_url": downloadURL(r, cv.Name, cv.Version), "dependencies": deps,
	}
}

func renderVersion(r *http.Request, cv *cookbookVersion) map[string]any {
	deps, plats := cv.Dependencies, cv.Platforms
	if deps == nil {
		deps = map[string]string{}
	}
	if plats == nil {
		plats = map[string]string{}
	}
	return map[string]any{
		"license": cv.License, "tarball_file_size": cv.Size, "version": cv.Version,
		"published_at": cv.PublishedAt, "average_rating": nil,
		"cookbook": cookbookURL(r, cv.Name), "file": downloadURL(r, cv.Name, cv.Version),
		"dependencies": deps, "platforms": plats, "quality_metrics": []any{},
	}
}

// renderCookbook builds the cookbook document from its versions (ascending).
func renderCookbook(r *http.Request, name string, cvs []*cookbookVersion) map[string]any {
	cur := cvs[len(cvs)-1]
	urls := make([]string, 0, len(cvs))
	for i := len(cvs) - 1; i >= 0; i-- {
		urls = append(urls, versionURL(r, name, cvs[i].Version))
	}
	category := cur.Category
	if category == "" {
		category = "Other"
	}
	return map[string]any{
		"name": name, "maintainer": cur.Maintainer, "description": cur.Description, "category": category,
		"latest_version": versionURL(r, name, cur.Version), "external_url": cur.SourceURL,
		"source_url": cur.SourceURL, "issues_url": cur.IssuesURL, "average_rating": nil,
		"created_at": cvs[0].PublishedAt, "updated_at": cur.PublishedAt, "up_for_adoption": nil,
		"deprecated": false, "versions": urls,
		"metrics": map[string]any{"downloads": map[string]any{"total": 0, "versions": map[string]int{}}, "followers": 0, "collaborators": 0},
	}
}

func listItem(r *http.Request, name string, cv *cookbookVersion) map[string]any {
	return map[string]any{
		"cookbook_name": name, "cookbook_maintainer": cv.Maintainer,
		"cookbook_description": cv.Description, "cookbook": cookbookURL(r, name),
	}
}

func pageParams(q url.Values) (int, int) {
	start, items := 0, 10
	if n, err := strconv.Atoi(q.Get("start")); err == nil && n > 0 {
		start = n
	}
	if n, err := strconv.Atoi(q.Get("items")); err == nil && n > 0 {
		items = min(n, 100)
	}
	return start, items
}

// ── shared read path ──

type backend interface {
	universe(ctx context.Context, r *http.Request) (map[string]map[string]any, error)
	versions(ctx context.Context, name string) ([]*cookbookVersion, error)
	version(ctx context.Context, name, v string) (*cookbookVersion, error)
	serveTarball(w http.ResponseWriter, r *http.Request, name, v string)
	serveListing(w http.ResponseWriter, r *http.Request, kind string)
}

func backendError(w http.ResponseWriter, err error) {
	switch {
	case pkgbase.IsNotFound(err):
		pkgbase.NotFound(w)
	case errors.Is(err, upstream.ErrTransient), errors.Is(err, upstream.ErrUnauthorized):
		pkgbase.UpstreamError(w, err)
	default:
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
	}
}

func serveRead(w http.ResponseWriter, r *http.Request, b backend) {
	rt, ok := parseRoute(r.Method, r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	ctx := r.Context()
	switch rt.kind {
	case "universe":
		u, err := b.universe(ctx, r)
		if err != nil {
			backendError(w, err)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, u)
	case "list", "search":
		b.serveListing(w, r, rt.kind)
	case "cookbook":
		cvs, err := b.versions(ctx, rt.name)
		if err != nil {
			backendError(w, err)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, renderCookbook(r, rt.name, cvs))
	case "version":
		cv, err := b.version(ctx, rt.name, rt.version)
		if err != nil {
			backendError(w, err)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, renderVersion(r, cv))
	case "download":
		b.serveTarball(w, r, rt.name, rt.version)
	default:
		pkgbase.NotFound(w)
	}
}

// ── tarball parsing ──

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		switch t := x.(type) {
		case string:
			out[k] = t
		case []any:
			// metadata.json may carry constraint arrays.
			var parts []string
			for _, p := range t {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			out[k] = strings.Join(parts, ", ")
		}
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// parseCookbook reads {dir}/metadata.json (or metadata.rb) from a
// cookbook tarball.
func parseCookbook(body []byte) (*cookbookVersion, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var metaJSON, metaRB []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		name := strings.Trim(path.Clean("/"+strings.TrimPrefix(hdr.Name, "./")), "/")
		if hdr.Typeflag != tar.TypeReg || strings.Count(name, "/") > 1 {
			continue
		}
		switch path.Base(name) {
		case "metadata.json":
			if metaJSON == nil {
				metaJSON, err = io.ReadAll(io.LimitReader(tr, 16<<20))
			}
		case "metadata.rb":
			if metaRB == nil {
				metaRB, err = io.ReadAll(io.LimitReader(tr, 16<<20))
			}
		}
		if err != nil {
			return nil, err
		}
	}
	var cv *cookbookVersion
	switch {
	case metaJSON != nil:
		var m map[string]any
		if err := json.Unmarshal(metaJSON, &m); err != nil {
			return nil, fmt.Errorf("parse metadata.json: %w", err)
		}
		cv = &cookbookVersion{
			Name: str(m["name"]), Version: str(m["version"]), License: str(m["license"]),
			Description: str(m["description"]), Maintainer: str(m["maintainer"]),
			SourceURL: str(m["source_url"]), IssuesURL: str(m["issues_url"]),
			Dependencies: strMap(m["dependencies"]), Platforms: strMap(m["platforms"]),
		}
	case metaRB != nil:
		cv = parseMetadataRB(string(metaRB))
	default:
		return nil, errors.New("metadata.json or metadata.rb not found in cookbook tarball")
	}
	if !validName(cv.Name) {
		return nil, errors.New("cookbook metadata must carry a valid name")
	}
	v, ok := normVersion(cv.Version)
	if !ok {
		return nil, fmt.Errorf("cookbook metadata carries an invalid version %q", cv.Version)
	}
	cv.Version = v
	return cv, nil
}

func parseMetadataRB(src string) *cookbookVersion {
	first := func(re *regexp.Regexp) string {
		if m := re.FindStringSubmatch(src); m != nil {
			return m[1]
		}
		return ""
	}
	cv := &cookbookVersion{
		Name: first(rbNameRe), Version: first(rbVerRe), License: first(rbLicRe),
		Description: first(rbDescRe), Maintainer: first(rbMaintRe),
		SourceURL: first(rbSrcRe), IssuesURL: first(rbIssRe), Dependencies: map[string]string{},
	}
	for _, m := range rbDependRe.FindAllStringSubmatch(src, -1) {
		c := m[2]
		if c == "" {
			c = ">= 0.0.0"
		}
		cv.Dependencies[m[1]] = c
	}
	return cv
}

// ── Store ──

// Store wraps pkgbase.Store with the cookbook layout:
// cookbooks/{name}/{version}/{version.json,<tarball>}.
type Store struct{ *pkgbase.Store }

func (s *Store) versionDir(n, v string) string { return path.Join(cbDir, n, v) }
func (s *Store) recordRel(n, v string) string  { return path.Join(s.versionDir(n, v), recordFile) }
func (s *Store) tarballRel(n, v string) string {
	return path.Join(s.versionDir(n, v), tarballName(n, v))
}

// Version returns the stored metadata of name@v.
func (s *Store) Version(n, v string) (*cookbookVersion, error) {
	b, err := s.Read(s.recordRel(n, v))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, registry.ErrPackageNotFound
		}
		return nil, err
	}
	var cv cookbookVersion
	if err := json.Unmarshal(b, &cv); err != nil {
		return nil, err
	}
	return &cv, nil
}

// Versions returns the stored versions of name (ascending).
func (s *Store) Versions(n string) ([]string, error) {
	dirs, err := s.ListDirs(path.Join(cbDir, n))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if s.Exists(s.recordRel(n, d)) {
			out = append(out, d)
		}
	}
	pkgbase.SortVersions(out)
	return out, nil
}

func (s *Store) all(n string) ([]*cookbookVersion, error) {
	vs, err := s.Versions(n)
	if err != nil {
		return nil, err
	}
	var out []*cookbookVersion
	for _, v := range vs {
		if cv, err := s.Version(n, v); err == nil {
			out = append(out, cv)
		}
	}
	if len(out) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	return out, nil
}

func (s *Store) put(cv *cookbookVersion, tarball []byte) error {
	if tarball != nil {
		if err := s.Write(s.tarballRel(cv.Name, cv.Version), tarball); err != nil {
			return err
		}
	}
	b, err := pkgbase.MarshalJSON(cv)
	if err != nil {
		return err
	}
	return s.Write(s.recordRel(cv.Name, cv.Version), b)
}

// ListPackages lists every stored cookbook.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := s.ListDirs(cbDir)
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, n := range names {
		if vs, _ := s.Versions(n); len(vs) > 0 {
			out = append(out, registry.PackageSummary{Name: n, Versions: vs})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteVersion removes name@version.
func (s *Store) DeleteVersion(_ context.Context, name, version string) error {
	v, ok := normVersion(version)
	if !validName(name) || !ok || !s.Exists(s.recordRel(name, v)) {
		return registry.ErrPackageNotFound
	}
	if _, _, errs := s.DeleteTree(s.versionDir(name, v)); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	cv, err := s.Version(ref.Name, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	t, _ := time.Parse(time.RFC3339, cv.PublishedAt)
	return registry.ArtifactMeta{License: cv.License, PublishedAt: t}, nil
}

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	if !validName(name) {
		return nil, registry.ErrInvalidPackageName
	}
	cvs, err := s.all(name)
	if err != nil {
		return nil, err
	}
	cur := cvs[len(cvs)-1]
	d := &registry.GenericPackageDetail{
		LatestVersion: cur.Version, Description: cur.Description, Homepage: cur.SourceURL,
		License: cur.License, Metadata: map[string]string{},
	}
	if cur.Maintainer != "" {
		d.Metadata["maintainer"] = cur.Maintainer
	}
	for i := len(cvs) - 1; i >= 0; i-- {
		cv := cvs[i]
		row := registry.GenericVersionDetail{Version: cv.Version, PublishedAt: cv.PublishedAt, Size: cv.Size, Metadata: map[string]string{}}
		if s.Exists(s.tarballRel(name, cv.Version)) {
			row.Files = []registry.GenericFile{{Name: tarballName(name, cv.Version), Size: cv.Size, SHA256: cv.SHA256}}
		}
		if len(cv.Dependencies) > 0 {
			parts := make([]string, 0, len(cv.Dependencies))
			for k, c := range cv.Dependencies {
				parts = append(parts, k+" "+c)
			}
			sort.Strings(parts)
			row.Metadata["dependencies"] = strings.Join(parts, ", ")
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

// localUniverse builds the universe from every stored version.
func localUniverse(ctx context.Context, s *Store, r *http.Request) (map[string]map[string]any, error) {
	pkgs, err := s.ListPackages(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]any, len(pkgs))
	for _, p := range pkgs {
		vers := map[string]any{}
		for _, v := range p.Versions {
			if cv, err := s.Version(p.Name, v); err == nil {
				vers[v] = universeEntry(r, cv)
			}
		}
		if len(vers) > 0 {
			out[p.Name] = vers
		}
	}
	return out, nil
}

// serveStoreListing answers /api/v1/cookbooks and /api/v1/search from
// the store.
func serveStoreListing(w http.ResponseWriter, r *http.Request, kind string, s *Store) {
	pkgs, err := s.ListPackages(r.Context())
	if err != nil {
		backendError(w, err)
		return
	}
	q := r.URL.Query()
	term := strings.ToLower(q.Get("q"))
	type hit struct {
		name string
		cv   *cookbookVersion
	}
	var hits []hit
	for _, p := range pkgs {
		cv, err := s.Version(p.Name, pkgbase.Latest(p.Versions))
		if err != nil {
			continue
		}
		if kind == "search" && term != "" && !strings.Contains(strings.ToLower(p.Name), term) && !strings.Contains(strings.ToLower(cv.Description), term) {
			continue
		}
		hits = append(hits, hit{p.Name, cv})
	}
	start, items := pageParams(q)
	out := make([]map[string]any, 0, items)
	for i := start; i < len(hits) && i < start+items; i++ {
		out = append(out, listItem(r, hits[i].name, hits[i].cv))
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"start": start, "total": len(hits), "items": out})
}

// ── Local ──

// Local is a hosted cookbook repository.
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

func (l *Local) universe(ctx context.Context, r *http.Request) (map[string]map[string]any, error) {
	return localUniverse(ctx, l.store, r)
}

func (l *Local) versions(_ context.Context, name string) ([]*cookbookVersion, error) {
	return l.store.all(name)
}

func (l *Local) version(_ context.Context, name, v string) (*cookbookVersion, error) {
	return l.store.Version(name, v)
}

func (l *Local) serveTarball(w http.ResponseWriter, r *http.Request, name, v string) {
	if !pkgbase.ServeStored(w, r, l.store.Store, l.store.tarballRel(name, v), "application/x-gzip") {
		pkgbase.NotFound(w)
	}
}

func (l *Local) serveListing(w http.ResponseWriter, r *http.Request, kind string) {
	serveStoreListing(w, r, kind, l.store)
}

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

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref)
}

// PromoteVersion copies name@version into dst (a local chef repo).
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("chef: promote target %s/%s is not a local chef repository", dst.Namespace(), dst.Name())
	}
	cv, err := l.store.Version(name, version)
	if err != nil {
		return err
	}
	body, err := l.store.Read(l.store.tarballRel(name, cv.Version))
	if err != nil {
		return err
	}
	if _, err := d.Guard.Check(d.store.Store, d.store.Exists(d.store.recordRel(name, cv.Version)), int64(len(body))); err != nil {
		return err
	}
	if err := d.store.put(cv, body); err != nil {
		return err
	}
	d.EmitPublished(name+"@"+cv.Version, int64(len(body)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveRead(w, r, l)
	case http.MethodPost, http.MethodPut:
		rt, ok := parseRoute(r.Method, r.URL.Path)
		if !ok || rt.kind != "publish" {
			pkgbase.NotFound(w)
			return
		}
		l.publish(w, r, rt)
	case http.MethodDelete:
		l.remove(w, r)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) publish(w http.ResponseWriter, r *http.Request, rt route) {
	if !l.CheckPush(w) {
		return
	}
	limit := l.MaxUpload
	if limit <= 0 {
		limit = pkgbase.DefaultMaxUpload
	}
	var body []byte
	var category string
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			pkgbase.Error(w, http.StatusBadRequest, "parse multipart: "+err.Error())
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, _, err := r.FormFile("tarball")
		if err != nil {
			pkgbase.Error(w, http.StatusBadRequest, "missing multipart field \"tarball\"")
			return
		}
		defer f.Close()
		if body, err = pkgbase.ReadBody(f, l.MaxUpload); err != nil {
			pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		var meta struct {
			Category string `json:"category"`
		}
		if json.Unmarshal([]byte(r.FormValue("cookbook")), &meta) == nil {
			category = meta.Category
		}
	} else {
		var err error
		if body, err = pkgbase.ReadBody(r.Body, l.MaxUpload); err != nil {
			pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
	}
	cv, err := parseCookbook(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if rt.name != "" && (rt.name != cv.Name || rt.version != cv.Version) {
		pkgbase.Error(w, http.StatusBadRequest, fmt.Sprintf("tarball metadata is %s@%s, URL says %s@%s", cv.Name, cv.Version, rt.name, rt.version))
		return
	}
	if !l.AllowPublish(w, l.store.Exists(l.store.recordRel(cv.Name, cv.Version)), int64(len(body))) {
		return
	}
	cv.Category = category
	cv.Size = int64(len(body))
	cv.SHA256 = pkgbase.SHA256Hex(body)
	cv.PublishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := l.store.put(cv, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(cv.Name+"@"+cv.Version, cv.Size)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]string{"uri": cookbookURL(r, cv.Name)})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	rt, ok := parseRoute(http.MethodGet, r.URL.Path)
	if !ok || (rt.kind != "version" && rt.kind != "cookbook") {
		pkgbase.NotFound(w)
		return
	}
	versions := []string{rt.version}
	if rt.kind == "cookbook" {
		versions, _ = l.store.Versions(rt.name)
		if len(versions) == 0 {
			pkgbase.NotFound(w)
			return
		}
	}
	for _, v := range versions {
		if err := l.store.DeleteVersion(r.Context(), rt.name, v); err != nil {
			backendError(w, err)
			return
		}
		l.EmitDeleted(rt.name + "@" + v)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote ──

// Remote is a pull-through proxy of a Supermarket.
type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/api/v1/cookbooks?items=1")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

// rewriteURLs replaces every upstream Supermarket API URL inside v
// with the kutu equivalent.
func rewriteURLs(v any, base string) any {
	switch t := v.(type) {
	case string:
		if m := apiURLRe.FindStringSubmatch(t); m != nil {
			return base + "/api/v1/" + m[1]
		}
		return t
	case []any:
		for i := range t {
			t[i] = rewriteURLs(t[i], base)
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = rewriteURLs(t[k], base)
		}
		return t
	}
	return v
}

func (rr *Remote) universe(ctx context.Context, r *http.Request) (map[string]map[string]any, error) {
	body, err := rr.FetchCached(ctx, path.Join(metaDir, "universe.json"), "/universe", true)
	if err != nil {
		return nil, err
	}
	var up map[string]map[string]map[string]any
	if err := json.Unmarshal(body, &up); err != nil {
		return nil, fmt.Errorf("parse upstream universe: %w", err)
	}
	out := make(map[string]map[string]any, len(up))
	for name, vers := range up {
		m := make(map[string]any, len(vers))
		for v, e := range vers {
			deps := e["dependencies"]
			if deps == nil {
				deps = map[string]any{}
			}
			m[v] = map[string]any{
				"location_type": "opscode", "location_path": apiBase(r),
				"download_url": downloadURL(r, name, v), "dependencies": deps,
			}
		}
		out[name] = m
	}
	return out, nil
}

type upstreamVersion struct {
	License      string            `json:"license"`
	Size         int64             `json:"tarball_file_size"`
	Version      string            `json:"version"`
	PublishedAt  string            `json:"published_at"`
	File         string            `json:"file"`
	Dependencies map[string]string `json:"dependencies"`
	Platforms    map[string]string `json:"platforms"`
}

func (rr *Remote) version(ctx context.Context, name, v string) (*cookbookVersion, error) {
	if cv, err := rr.store.Version(name, v); err == nil {
		return cv, nil
	}
	resp, err := rr.Client.Get(ctx, "/api/v1/cookbooks/"+url.PathEscape(name)+"/versions/"+v)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var u upstreamVersion
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("parse upstream version: %w", err)
	}
	cv := &cookbookVersion{
		Name: name, Version: v, License: u.License, Size: u.Size, PublishedAt: u.PublishedAt,
		Dependencies: u.Dependencies, Platforms: u.Platforms, UpstreamFile: u.File,
	}
	if cb, err := rr.cookbookDoc(ctx, name); err == nil {
		cv.Description, cv.Maintainer = str(cb["description"]), str(cb["maintainer"])
		cv.SourceURL, cv.IssuesURL, cv.Category = str(cb["source_url"]), str(cb["issues_url"]), str(cb["category"])
	}
	_ = rr.store.put(cv, nil)
	return cv, nil
}

func (rr *Remote) cookbookDoc(ctx context.Context, name string) (map[string]any, error) {
	body, err := rr.FetchCached(ctx, path.Join(metaDir, "cookbooks", name+".json"), "/api/v1/cookbooks/"+url.PathEscape(name), true)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse upstream cookbook: %w", err)
	}
	return doc, nil
}

// versions lists the upstream versions (metadata of versions not yet
// pulled is filled from the cookbook document only).
func (rr *Remote) versions(ctx context.Context, name string) ([]*cookbookVersion, error) {
	doc, err := rr.cookbookDoc(ctx, name)
	if err != nil {
		return nil, err
	}
	urls, _ := doc["versions"].([]any)
	var out []*cookbookVersion
	for _, u := range urls {
		s := strings.TrimSuffix(str(u), "/")
		v, ok := normVersion(s[strings.LastIndex(s, "/")+1:])
		if !ok {
			continue
		}
		cv, err := rr.store.Version(name, v)
		if err != nil {
			cv = &cookbookVersion{Name: name, Version: v}
		}
		cv.Description, cv.Maintainer = str(doc["description"]), str(doc["maintainer"])
		cv.SourceURL, cv.IssuesURL, cv.Category = str(doc["source_url"]), str(doc["issues_url"]), str(doc["category"])
		out = append(out, cv)
	}
	if len(out) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	sort.SliceStable(out, func(i, j int) bool { return pkgbase.CompareVersions(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

func (rr *Remote) fetchTarball(ctx context.Context, cv *cookbookVersion) ([]byte, error) {
	src := cv.UpstreamFile
	if src == "" {
		src = "/api/v1/cookbooks/" + url.PathEscape(cv.Name) + "/versions/" + cv.Version + "/download"
	}
	resp, err := rr.Client.Get(ctx, src)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = rr.store.Write(rr.store.tarballRel(cv.Name, cv.Version), body)
	if cv.SHA256 == "" {
		cv.SHA256 = pkgbase.SHA256Hex(body)
		cv.Size = int64(len(body))
		_ = rr.store.put(cv, nil)
	}
	return body, nil
}

func (rr *Remote) serveTarball(w http.ResponseWriter, r *http.Request, name, v string) {
	if pkgbase.ServeStored(w, r, rr.store.Store, rr.store.tarballRel(name, v), "application/x-gzip") {
		return
	}
	cv, err := rr.version(r.Context(), name, v)
	if err != nil {
		backendError(w, err)
		return
	}
	body, err := rr.fetchTarball(r.Context(), cv)
	if err != nil {
		backendError(w, err)
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, "application/x-gzip", body)
}

func (rr *Remote) serveListing(w http.ResponseWriter, r *http.Request, kind string) {
	p := "/api/v1/cookbooks"
	if kind == "search" {
		p = "/api/v1/search"
	}
	q := r.URL.Query()
	start, items := pageParams(q)
	uq := url.Values{"start": {strconv.Itoa(start)}, "items": {strconv.Itoa(items)}}
	if kind == "search" {
		uq.Set("q", q.Get("q"))
	}
	for _, k := range []string{"order", "user"} {
		if v := q.Get(k); v != "" {
			uq.Set(k, v)
		}
	}
	upath := p + "?" + uq.Encode()
	body, err := rr.FetchCached(r.Context(), path.Join(metaDir, "listings", pkgbase.SHA256Hex([]byte(upath))+".json"), upath, true)
	if err != nil {
		backendError(w, err)
		return
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		pkgbase.Error(w, http.StatusBadGateway, "upstream: invalid listing JSON")
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, rewriteURLs(doc, pkgbase.PublicBase(r)))
}

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
	return rr.Purge(opts, metaDir), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	v, ok := normVersion(ref.Version)
	if !validName(ref.Name) || !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	if _, err := rr.version(ctx, ref.Name, v); err != nil {
		return registry.ArtifactMeta{}, err
	}
	return artifactInfo(rr.store, registry.ArtifactRef{Name: ref.Name, Version: v})
}

// Prefetch warms the cookbook document, version metadata and tarball.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if !validName(name) {
		return registry.ErrInvalidPackageName
	}
	if version == "" {
		cvs, err := rr.versions(ctx, name)
		if err != nil {
			return err
		}
		version = cvs[len(cvs)-1].Version
	}
	v, ok := normVersion(version)
	if !ok {
		return registry.ErrPackageNotFound
	}
	cv, err := rr.version(ctx, name, v)
	if err != nil {
		return err
	}
	if rr.store.Exists(rr.store.tarballRel(name, v)) {
		return nil
	}
	_, err = rr.fetchTarball(ctx, cv)
	return err
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	serveRead(w, r, rr)
}

// ── Virtual ──

// Virtual merges the universe and cookbook version lists across
// members; version documents, tarballs and listings are first-hit.
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
	serveRead(w, r, &virtualView{v: v, r: r})
}

type virtualView struct {
	v *Virtual
	r *http.Request
}

func (vv *virtualView) members(fn func(backend) bool) {
	vv.v.ForEachMember(func(reg registry.Registry) bool {
		b, ok := reg.(backend)
		if !ok {
			return false
		}
		if _, _, ok := registry.CheckGate(reg, vv.r); !ok {
			return false
		}
		return fn(b)
	})
}

func (vv *virtualView) universe(ctx context.Context, r *http.Request) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	vv.members(func(b backend) bool {
		u, err := b.universe(ctx, r)
		if err != nil {
			return false
		}
		for name, vers := range u {
			dst, ok := out[name]
			if !ok {
				dst = map[string]any{}
				out[name] = dst
			}
			for ver, e := range vers {
				if _, seen := dst[ver]; !seen {
					dst[ver] = e
				}
			}
		}
		return false
	})
	return out, nil
}

func (vv *virtualView) versions(ctx context.Context, name string) ([]*cookbookVersion, error) {
	seen := map[string]bool{}
	var out []*cookbookVersion
	vv.members(func(b backend) bool {
		cvs, err := b.versions(ctx, name)
		if err != nil {
			return false
		}
		for _, cv := range cvs {
			if !seen[cv.Version] {
				seen[cv.Version] = true
				out = append(out, cv)
			}
		}
		return false
	})
	if len(out) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	sort.SliceStable(out, func(i, j int) bool { return pkgbase.CompareVersions(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

func (vv *virtualView) version(ctx context.Context, name, ver string) (*cookbookVersion, error) {
	var found *cookbookVersion
	vv.members(func(b backend) bool {
		cv, err := b.version(ctx, name, ver)
		if err == nil {
			found = cv
			return true
		}
		return false
	})
	if found == nil {
		return nil, registry.ErrPackageNotFound
	}
	return found, nil
}

func (vv *virtualView) serveTarball(w http.ResponseWriter, r *http.Request, _, _ string) {
	if !vv.v.ServeFirstHit(w, r) {
		pkgbase.NotFound(w)
	}
}

func (vv *virtualView) serveListing(w http.ResponseWriter, r *http.Request, _ string) {
	if !vv.v.ServeFirstHit(w, r) {
		pkgbase.NotFound(w)
	}
}
