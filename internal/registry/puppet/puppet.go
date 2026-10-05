// Package puppet implements a Puppet Forge v3 API subset used by
// `puppet module install` and r10k / forge-ruby:
//
//	GET    /v3/modules                                    paginated module list (?query=)
//	GET    /v3/modules/{owner}-{name}                     module with current_release + releases
//	GET    /v3/releases?module={owner}-{name}             paginated releases (limit, offset)
//	GET    /v3/releases/{owner}-{name}-{version}          release detail (metadata, checksums)
//	GET    /v3/files/{owner}-{name}-{version}.tar.gz      module tarball
//	POST   /v3/releases                                   publish (multipart field "file", local)
//	DELETE /v3/releases/{owner}-{name}-{version}          delete a release (local)
//
// file_uri and pagination links are relative to the forge base URL
// (the repo URL), matching how both puppet and forge-ruby resolve
// them. Remote repos proxy a Forge API (URL e.g.
// https://forgeapi.puppet.com): release lists honour MutableTTL,
// release metadata and tarballs are cached forever.
//
// Client configuration:
//
//	puppet module install acme-foo --module_repository https://kutu.example.com/registries/{ns}/{repo}
//	# puppet.conf: forge_authorization = Bearer <kutu token>
//
//	# r10k.yaml
//	forge:
//	  baseurl: https://kutu.example.com/registries/{ns}/{repo}
//	  authorization_token: 'Bearer <kutu token>'
//
//	# publish
//	curl -H "Authorization: Bearer <token>" -F file=@acme-foo-1.0.0.tar.gz \
//	  https://kutu.example.com/registries/{ns}/{repo}/v3/releases
package puppet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec // Forge protocol checksum
	"encoding/hex"
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

const typ = service.RegistryTypePuppet

const (
	modDir     = "modules"
	metaDir    = "meta"
	recordFile = "release.json"
	timeLayout = "2006-01-02 15:04:05 -0700"
)

var partRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// release is the stored record of one module release.
type release struct {
	Owner      string         `json:"owner"`
	Name       string         `json:"name"`
	Version    string         `json:"version"`
	FileMD5    string         `json:"file_md5,omitempty"`
	FileSHA256 string         `json:"file_sha256,omitempty"`
	FileSize   int64          `json:"file_size,omitempty"`
	CreatedAt  string         `json:"created_at,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	// UpstreamFile is the upstream file_uri (remote only).
	UpstreamFile string `json:"upstream_file,omitempty"`
}

func (r *release) slug() string { return r.Owner + "-" + r.Name + "-" + r.Version }

func validVersion(v string) bool {
	return v != "" && v != "." && v != ".." && !strings.ContainsAny(v, "/\\\x00")
}

// splitModule accepts "owner-name" or "owner/name".
func splitModule(s string) (string, string, bool) {
	s = strings.Trim(s, "/")
	i := strings.IndexAny(s, "-/")
	if i <= 0 {
		return "", "", false
	}
	owner, name := s[:i], s[i+1:]
	if !partRe.MatchString(owner) || !partRe.MatchString(name) {
		return "", "", false
	}
	return owner, name, true
}

// splitRelease parses "owner-name-version".
func splitRelease(s string) (owner, name, version string, ok bool) {
	parts := strings.SplitN(s, "-", 3)
	if len(parts) != 3 || !partRe.MatchString(parts[0]) || !partRe.MatchString(parts[1]) || !validVersion(parts[2]) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := r.URL.Path
	if f, ok := strings.CutPrefix(p, "/v3/files/"); ok {
		if o, n, v, ok := splitRelease(strings.TrimSuffix(f, ".tar.gz")); ok {
			return registry.ArtifactRef{Name: o + "-" + n, Version: v}, true
		}
		return registry.ArtifactRef{}, false
	}
	if s, ok := strings.CutPrefix(p, "/v3/modules/"); ok {
		if o, n, ok := splitModule(s); ok {
			return registry.ArtifactRef{Name: o + "-" + n}, true
		}
	}
	if s, ok := strings.CutPrefix(p, "/v3/releases/"); ok {
		if o, n, _, ok := splitRelease(s); ok {
			return registry.ArtifactRef{Name: o + "-" + n}, true
		}
	}
	if p == "/v3/releases" {
		if o, n, ok := splitModule(r.URL.Query().Get("module")); ok {
			return registry.ArtifactRef{Name: o + "-" + n}, true
		}
	}
	return registry.ArtifactRef{}, false
}

// ── rendering ──

func owner(o string) map[string]any {
	return map[string]any{"uri": "/v3/users/" + o, "slug": o, "username": o, "gravatar_id": nil}
}

func moduleRef(o, n string) map[string]any {
	return map[string]any{"uri": "/v3/modules/" + o + "-" + n, "slug": o + "-" + n, "name": n, "deprecated_at": nil, "owner": owner(o)}
}

func fileURI(rel *release) string { return "/v3/files/" + rel.slug() + ".tar.gz" }

func renderRelease(rel *release) map[string]any {
	meta := rel.Metadata
	if meta == nil {
		meta = map[string]any{"name": rel.Owner + "-" + rel.Name, "version": rel.Version, "dependencies": []any{}}
	}
	return map[string]any{
		"uri": "/v3/releases/" + rel.slug(), "slug": rel.slug(), "module": moduleRef(rel.Owner, rel.Name),
		"version": rel.Version, "metadata": meta, "tags": []any{}, "supported": false, "pdk": false,
		"validation_score": nil, "file_uri": fileURI(rel), "file_size": rel.FileSize,
		"file_md5": rel.FileMD5, "file_sha256": rel.FileSHA256, "downloads": 0,
		"readme": nil, "changelog": nil, "license": nil, "reference": nil, "tasks": []any{}, "plans": []any{},
		"created_at": rel.CreatedAt, "updated_at": rel.CreatedAt, "deleted_at": nil, "deleted_for": nil,
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// renderModule builds a module document from its releases (ascending).
func renderModule(o, n string, rels []*release) map[string]any {
	cur := rels[len(rels)-1]
	short := make([]map[string]any, 0, len(rels))
	for i := len(rels) - 1; i >= 0; i-- {
		r := rels[i]
		short = append(short, map[string]any{
			"uri": "/v3/releases/" + r.slug(), "slug": r.slug(), "version": r.Version, "supported": false,
			"created_at": r.CreatedAt, "deleted_at": nil, "file_uri": fileURI(r), "file_size": r.FileSize,
		})
	}
	return map[string]any{
		"uri": "/v3/modules/" + o + "-" + n, "slug": o + "-" + n, "name": n, "downloads": 0,
		"created_at": rels[0].CreatedAt, "updated_at": cur.CreatedAt, "deprecated_at": nil, "deprecated_for": nil,
		"superseded_by": nil, "supported": false, "endorsement": nil, "module_group": "base",
		"owner": owner(o), "premium": false, "current_release": renderRelease(cur), "releases": short,
		"feedback_score": nil, "homepage_url": str(cur.Metadata["project_page"]), "issues_url": str(cur.Metadata["issues_url"]),
	}
}

func pageParams(q url.Values) (int, int) {
	limit, offset := 20, 0
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 100)
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		offset = n
	}
	return limit, offset
}

func pagination(p string, q url.Values, limit, offset, total int) map[string]any {
	link := func(o int) string {
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		qq.Set("limit", strconv.Itoa(limit))
		qq.Set("offset", strconv.Itoa(o))
		return p + "?" + qq.Encode()
	}
	out := map[string]any{
		"limit": limit, "offset": offset, "first": link(0), "previous": nil,
		"current": link(offset), "next": nil, "total": total,
	}
	if offset > 0 {
		out["previous"] = link(max(0, offset-limit))
	}
	if offset+limit < total {
		out["next"] = link(offset + limit)
	}
	return out
}

func page[T any](items []T, limit, offset int) []T {
	if offset >= len(items) {
		return []T{}
	}
	return items[offset:min(len(items), offset+limit)]
}

// ── shared read path ──

type backend interface {
	releases(ctx context.Context, owner, name string) ([]*release, error)
	release(ctx context.Context, owner, name, version string) (*release, error)
	serveFile(w http.ResponseWriter, r *http.Request, owner, name, version string)
	modules(ctx context.Context) ([]registry.PackageSummary, error)
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
	ctx := r.Context()
	p := strings.TrimSuffix(r.URL.Path, "/")
	q := r.URL.Query()
	switch {
	case p == "/v3/releases":
		var rels []*release
		if m := q.Get("module"); m != "" {
			o, n, ok := splitModule(m)
			if !ok {
				pkgbase.Error(w, http.StatusBadRequest, "invalid module")
				return
			}
			var err error
			if rels, err = b.releases(ctx, o, n); err != nil && !pkgbase.IsNotFound(err) {
				backendError(w, err)
				return
			}
		}
		newest := make([]*release, len(rels))
		for i, rel := range rels {
			newest[len(rels)-1-i] = rel
		}
		limit, offset := pageParams(q)
		results := make([]map[string]any, 0, limit)
		for _, rel := range page(newest, limit, offset) {
			results = append(results, renderRelease(rel))
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"pagination": pagination(p, q, limit, offset, len(newest)), "results": results})
	case strings.HasPrefix(p, "/v3/releases/"):
		o, n, v, ok := splitRelease(strings.TrimPrefix(p, "/v3/releases/"))
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		rel, err := b.release(ctx, o, n, v)
		if err != nil {
			backendError(w, err)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, renderRelease(rel))
	case p == "/v3/modules":
		mods, err := b.modules(ctx)
		if err != nil {
			backendError(w, err)
			return
		}
		query := strings.ToLower(q.Get("query"))
		var matched []registry.PackageSummary
		for _, m := range mods {
			if query == "" || strings.Contains(strings.ToLower(m.Name), query) {
				matched = append(matched, m)
			}
		}
		limit, offset := pageParams(q)
		results := make([]map[string]any, 0, limit)
		for _, m := range page(matched, limit, offset) {
			o, n, ok := splitModule(m.Name)
			if !ok {
				continue
			}
			if rels, err := b.releases(ctx, o, n); err == nil && len(rels) > 0 {
				results = append(results, renderModule(o, n, rels))
			}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"pagination": pagination(p, q, limit, offset, len(matched)), "results": results})
	case strings.HasPrefix(p, "/v3/modules/"):
		o, n, ok := splitModule(strings.TrimPrefix(p, "/v3/modules/"))
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		rels, err := b.releases(ctx, o, n)
		if err == nil && len(rels) == 0 {
			err = registry.ErrPackageNotFound
		}
		if err != nil {
			backendError(w, err)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, renderModule(o, n, rels))
	case strings.HasPrefix(p, "/v3/files/"):
		o, n, v, ok := splitRelease(strings.TrimSuffix(strings.TrimPrefix(p, "/v3/files/"), ".tar.gz"))
		if !ok || !strings.HasSuffix(p, ".tar.gz") {
			pkgbase.NotFound(w)
			return
		}
		b.serveFile(w, r, o, n, v)
	default:
		pkgbase.NotFound(w)
	}
}

// ── tarball parsing ──

// parseModule reads {dir}/metadata.json from a module tarball.
func parseModule(body []byte) (*release, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("metadata.json not found in module tarball")
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		name := strings.Trim(path.Clean("/"+strings.TrimPrefix(hdr.Name, "./")), "/")
		if hdr.Typeflag != tar.TypeReg || path.Base(name) != "metadata.json" || strings.Count(name, "/") > 1 {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, err
		}
		var meta map[string]any
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("parse metadata.json: %w", err)
		}
		o, n, ok := splitModule(str(meta["name"]))
		v := str(meta["version"])
		if !ok || !validVersion(v) {
			return nil, errors.New("metadata.json must carry a valid \"owner-name\" name and a version")
		}
		meta["name"] = o + "-" + n
		if meta["dependencies"] == nil {
			meta["dependencies"] = []any{}
		}
		return &release{Owner: o, Name: n, Version: v, Metadata: meta}, nil
	}
}

// ── Store ──

// Store wraps pkgbase.Store with the module layout:
// modules/{owner}-{name}/{version}/{release.json,<tarball>}.
type Store struct{ *pkgbase.Store }

func (s *Store) versionDir(o, n, v string) string { return path.Join(modDir, o+"-"+n, v) }
func (s *Store) recordRel(o, n, v string) string {
	return path.Join(s.versionDir(o, n, v), recordFile)
}
func (s *Store) fileRel(o, n, v string) string {
	return path.Join(s.versionDir(o, n, v), o+"-"+n+"-"+v+".tar.gz")
}

// Release returns the stored record of owner-name@v.
func (s *Store) Release(o, n, v string) (*release, error) {
	b, err := s.Read(s.recordRel(o, n, v))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, registry.ErrPackageNotFound
		}
		return nil, err
	}
	var rel release
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// Versions returns the stored versions (ascending).
func (s *Store) Versions(o, n string) ([]string, error) {
	dirs, err := s.ListDirs(path.Join(modDir, o+"-"+n))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if s.Exists(s.recordRel(o, n, d)) {
			out = append(out, d)
		}
	}
	pkgbase.SortVersions(out)
	return out, nil
}

func (s *Store) releases(o, n string) ([]*release, error) {
	vs, err := s.Versions(o, n)
	if err != nil {
		return nil, err
	}
	var out []*release
	for _, v := range vs {
		if rel, err := s.Release(o, n, v); err == nil {
			out = append(out, rel)
		}
	}
	if len(out) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	return out, nil
}

func (s *Store) put(rel *release, tarball []byte) error {
	if tarball != nil {
		if err := s.Write(s.fileRel(rel.Owner, rel.Name, rel.Version), tarball); err != nil {
			return err
		}
	}
	b, err := pkgbase.MarshalJSON(rel)
	if err != nil {
		return err
	}
	return s.Write(s.recordRel(rel.Owner, rel.Name, rel.Version), b)
}

// ListPackages lists every stored module as "owner-name".
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	dirs, err := s.ListDirs(modDir)
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, d := range dirs {
		o, n, ok := splitModule(d)
		if !ok {
			continue
		}
		if vs, _ := s.Versions(o, n); len(vs) > 0 {
			out = append(out, registry.PackageSummary{Name: d, Versions: vs})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteVersion removes pkg ("owner-name") @ version.
func (s *Store) DeleteVersion(_ context.Context, pkg, version string) error {
	o, n, ok := splitModule(pkg)
	if !ok || !validVersion(version) || !s.Exists(s.recordRel(o, n, version)) {
		return registry.ErrPackageNotFound
	}
	if _, _, errs := s.DeleteTree(s.versionDir(o, n, version)); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func parseTime(s string) time.Time {
	for _, l := range []string{timeLayout, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	o, n, ok := splitModule(ref.Name)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	rel, err := s.Release(o, n, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	return registry.ArtifactMeta{License: str(rel.Metadata["license"]), PublishedAt: parseTime(rel.CreatedAt)}, nil
}

func detail(s *Store, pkg string) (*registry.PackageDetail, error) {
	o, n, ok := splitModule(pkg)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	rels, err := s.releases(o, n)
	if err != nil {
		return nil, err
	}
	cur := rels[len(rels)-1]
	d := &registry.GenericPackageDetail{
		LatestVersion: cur.Version,
		Description:   str(cur.Metadata["summary"]),
		Homepage:      str(cur.Metadata["project_page"]),
		License:       str(cur.Metadata["license"]),
		Metadata:      map[string]string{"owner": o},
	}
	if src := str(cur.Metadata["source"]); src != "" {
		d.Metadata["source"] = src
	}
	for i := len(rels) - 1; i >= 0; i-- {
		rel := rels[i]
		row := registry.GenericVersionDetail{Version: rel.Version, Size: rel.FileSize, Metadata: map[string]string{}}
		if t := parseTime(rel.CreatedAt); !t.IsZero() {
			row.PublishedAt = t.UTC().Format(time.RFC3339)
		}
		if s.Exists(s.fileRel(o, n, rel.Version)) {
			row.Files = []registry.GenericFile{{Name: rel.slug() + ".tar.gz", Size: rel.FileSize, SHA256: rel.FileSHA256}}
		}
		if deps, ok := rel.Metadata["dependencies"].([]any); ok && len(deps) > 0 {
			var parts []string
			for _, dep := range deps {
				if m, ok := dep.(map[string]any); ok {
					parts = append(parts, strings.TrimSpace(str(m["name"])+" "+str(m["version_requirement"])))
				}
			}
			row.Metadata["dependencies"] = strings.Join(parts, ", ")
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: o + "-" + n, Generic: d}, nil
}

// ── Local ──

// Local is a hosted Forge repository.
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

func (l *Local) releases(_ context.Context, o, n string) ([]*release, error) {
	return l.store.releases(o, n)
}

func (l *Local) release(_ context.Context, o, n, v string) (*release, error) {
	return l.store.Release(o, n, v)
}

func (l *Local) serveFile(w http.ResponseWriter, r *http.Request, o, n, v string) {
	if !pkgbase.ServeStored(w, r, l.store.Store, l.store.fileRel(o, n, v), "application/gzip") {
		pkgbase.NotFound(w)
	}
}

func (l *Local) modules(ctx context.Context) ([]registry.PackageSummary, error) {
	return l.store.ListPackages(ctx)
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

// PromoteVersion copies name@version into dst (a local puppet repo).
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("puppet: promote target %s/%s is not a local puppet repository", dst.Namespace(), dst.Name())
	}
	o, n, ok := splitModule(name)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	rel, err := l.store.Release(o, n, version)
	if err != nil {
		return err
	}
	body, err := l.store.Read(l.store.fileRel(o, n, version))
	if err != nil {
		return err
	}
	if _, err := d.Guard.Check(d.store.Store, d.store.Exists(d.store.recordRel(o, n, version)), int64(len(body))); err != nil {
		return err
	}
	if err := d.store.put(rel, body); err != nil {
		return err
	}
	d.EmitPublished(rel.slug(), int64(len(body)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSuffix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveRead(w, r, l)
	case http.MethodPost, http.MethodPut:
		if p != "/v3/releases" {
			pkgbase.NotFound(w)
			return
		}
		l.publish(w, r)
	case http.MethodDelete:
		l.remove(w, r, p)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) publish(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	limit := l.MaxUpload
	if limit <= 0 {
		limit = pkgbase.DefaultMaxUpload
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, _, err := r.FormFile("file")
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "missing multipart field \"file\"")
		return
	}
	defer f.Close()
	body, err := pkgbase.ReadBody(f, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	rel, err := parseModule(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if !l.AllowPublish(w, l.store.Exists(l.store.recordRel(rel.Owner, rel.Name, rel.Version)), int64(len(body))) {
		return
	}
	sum := md5.Sum(body) //nolint:gosec
	rel.FileMD5 = hex.EncodeToString(sum[:])
	rel.FileSHA256 = pkgbase.SHA256Hex(body)
	rel.FileSize = int64(len(body))
	rel.CreatedAt = time.Now().UTC().Format(timeLayout)
	if err := l.store.put(rel, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(rel.slug(), rel.FileSize)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]string{"uri": "/v3/releases/" + rel.slug(), "slug": rel.slug()})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request, p string) {
	if !l.CheckPush(w) {
		return
	}
	o, n, v, ok := splitRelease(strings.TrimPrefix(p, "/v3/releases/"))
	if !ok || !strings.HasPrefix(p, "/v3/releases/") {
		pkgbase.NotFound(w)
		return
	}
	if err := l.store.DeleteVersion(r.Context(), o+"-"+n, v); err != nil {
		backendError(w, err)
		return
	}
	l.EmitDeleted(o + "-" + n + "-" + v)
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote ──

// Remote is a pull-through proxy of a Forge API.
type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/v3/modules?limit=1")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

const excludeFields = "readme changelog license reference"

type upstreamRelease struct {
	Version    string         `json:"version"`
	Metadata   map[string]any `json:"metadata"`
	FileURI    string         `json:"file_uri"`
	FileSize   int64          `json:"file_size"`
	FileMD5    string         `json:"file_md5"`
	FileSHA256 string         `json:"file_sha256"`
	CreatedAt  string         `json:"created_at"`
}

func (u *upstreamRelease) toRelease(o, n string) *release {
	return &release{
		Owner: o, Name: n, Version: u.Version, FileMD5: u.FileMD5, FileSHA256: u.FileSHA256,
		FileSize: u.FileSize, CreatedAt: u.CreatedAt, Metadata: u.Metadata, UpstreamFile: u.FileURI,
	}
}

func (rr *Remote) get(ctx context.Context, p string) ([]byte, error) {
	resp, err := rr.Client.Get(ctx, p)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (rr *Remote) fetchReleases(ctx context.Context, o, n string) ([]*release, error) {
	q := url.Values{"module": {o + "-" + n}, "limit": {"100"}, "exclude_fields": {excludeFields}}
	next := "/v3/releases?" + q.Encode()
	var out []*release
	for i := 0; next != "" && i < 200; i++ {
		body, err := rr.get(ctx, next)
		if err != nil {
			return nil, err
		}
		var pg struct {
			Pagination struct {
				Next string `json:"next"`
			} `json:"pagination"`
			Results []upstreamRelease `json:"results"`
		}
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, fmt.Errorf("parse upstream releases: %w", err)
		}
		for i := range pg.Results {
			if validVersion(pg.Results[i].Version) {
				out = append(out, pg.Results[i].toRelease(o, n))
			}
		}
		next = pg.Pagination.Next
	}
	sort.SliceStable(out, func(i, j int) bool { return pkgbase.CompareVersions(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

func (rr *Remote) releases(ctx context.Context, o, n string) ([]*release, error) {
	cache := path.Join(metaDir, o+"-"+n, "releases.json")
	readCache := func() ([]*release, error) {
		b, err := rr.store.Read(cache)
		if err != nil {
			return nil, err
		}
		var out []*release
		return out, json.Unmarshal(b, &out)
	}
	if rr.Fresh(cache) {
		if out, err := readCache(); err == nil {
			return out, nil
		}
	}
	out, err := rr.fetchReleases(ctx, o, n)
	if err != nil {
		if !errors.Is(err, upstream.ErrNotFound) {
			if cached, cerr := readCache(); cerr == nil {
				return cached, nil
			}
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	if b, err := pkgbase.MarshalJSON(out); err == nil {
		_ = rr.store.Write(cache, b)
	}
	return out, nil
}

func (rr *Remote) release(ctx context.Context, o, n, v string) (*release, error) {
	if rel, err := rr.store.Release(o, n, v); err == nil {
		return rel, nil
	}
	body, err := rr.get(ctx, "/v3/releases/"+o+"-"+n+"-"+v+"?"+url.Values{"exclude_fields": {excludeFields}}.Encode())
	if err != nil {
		return nil, err
	}
	var u upstreamRelease
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("parse upstream release: %w", err)
	}
	if u.Version == "" {
		u.Version = v
	}
	rel := u.toRelease(o, n)
	_ = rr.store.put(rel, nil)
	return rel, nil
}

func (rr *Remote) fetchFile(ctx context.Context, rel *release) ([]byte, error) {
	src := rel.UpstreamFile
	if src == "" {
		src = fileURI(rel)
	}
	body, err := rr.get(ctx, src)
	if err != nil {
		return nil, err
	}
	if rel.FileSHA256 != "" && !strings.EqualFold(rel.FileSHA256, pkgbase.SHA256Hex(body)) {
		return nil, fmt.Errorf("upstream file sha256 mismatch for %s: %w", rel.slug(), upstream.ErrTransient)
	}
	_ = rr.store.Write(rr.store.fileRel(rel.Owner, rel.Name, rel.Version), body)
	return body, nil
}

func (rr *Remote) serveFile(w http.ResponseWriter, r *http.Request, o, n, v string) {
	if pkgbase.ServeStored(w, r, rr.store.Store, rr.store.fileRel(o, n, v), "application/gzip") {
		return
	}
	rel, err := rr.release(r.Context(), o, n, v)
	if err != nil {
		backendError(w, err)
		return
	}
	body, err := rr.fetchFile(r.Context(), rel)
	if err != nil {
		backendError(w, err)
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, "application/gzip", body)
}

func (rr *Remote) modules(ctx context.Context) ([]registry.PackageSummary, error) {
	return rr.store.ListPackages(ctx)
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
	o, n, ok := splitModule(ref.Name)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	if _, err := rr.release(ctx, o, n, ref.Version); err != nil {
		return registry.ArtifactMeta{}, err
	}
	return artifactInfo(rr.store, ref)
}

// Prefetch warms the release list, release metadata and tarball.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	o, n, ok := splitModule(name)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	if version == "" {
		rels, err := rr.releases(ctx, o, n)
		if err != nil {
			return err
		}
		version = rels[len(rels)-1].Version
	}
	rel, err := rr.release(ctx, o, n, version)
	if err != nil {
		return err
	}
	if rr.store.Exists(rr.store.fileRel(o, n, version)) {
		return nil
	}
	_, err = rr.fetchFile(ctx, rel)
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

// Virtual merges release lists across members; release metadata and
// files are served first-hit.
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

func (vv *virtualView) releases(ctx context.Context, o, n string) ([]*release, error) {
	seen := map[string]bool{}
	var out []*release
	vv.members(func(b backend) bool {
		rels, err := b.releases(ctx, o, n)
		if err != nil {
			return false
		}
		for _, rel := range rels {
			if !seen[rel.Version] {
				seen[rel.Version] = true
				out = append(out, rel)
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

func (vv *virtualView) release(ctx context.Context, o, n, ver string) (*release, error) {
	var found *release
	vv.members(func(b backend) bool {
		rel, err := b.release(ctx, o, n, ver)
		if err == nil {
			found = rel
			return true
		}
		return false
	})
	if found == nil {
		return nil, registry.ErrPackageNotFound
	}
	return found, nil
}

func (vv *virtualView) serveFile(w http.ResponseWriter, r *http.Request, _, _, _ string) {
	if !vv.v.ServeFirstHit(w, r) {
		pkgbase.NotFound(w)
	}
}

func (vv *virtualView) modules(ctx context.Context) ([]registry.PackageSummary, error) {
	return vv.v.ListPackages(ctx)
}
