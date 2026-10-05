// Package ansible implements an Ansible Galaxy collection registry
// (the Galaxy v3 / Galaxy NG subset used by ansible-galaxy):
//
//	GET    /api/                                            API root (available_versions)
//	GET    /api/v3/                                         v3 root
//	GET    /api/v3/collections/                             paginated collection list
//	GET    /api/v3/collections/{ns}/{name}/                 collection detail
//	GET    /api/v3/collections/{ns}/{name}/versions/        paginated version list
//	GET    /api/v3/collections/{ns}/{name}/versions/{v}/    version detail
//	POST   /api/v3/artifacts/collections/                   publish (multipart: file, sha256)
//	GET    /api/v3/imports/collections/{id}/                import task status
//	GET    /download/{ns}-{name}-{v}.tar.gz                 collection tarball
//	DELETE /api/v3/collections/{ns}/{name}/versions/{v}/    delete a version (local)
//	DELETE /api/v3/collections/{ns}/{name}/                 delete every version (local)
//
// The Galaxy NG content paths
// (/api/v3/plugin/ansible/content/{distro}/collections/index/...,
// .../collections/artifacts/{file} and /api/content/{distro}/v3/...)
// are accepted as aliases. Remote repos proxy a Galaxy server (URL is
// the server root, e.g. https://galaxy.ansible.com), cache version
// metadata and tarballs forever and refresh version lists after
// MutableTTL; download_url is always rewritten to point at kutu.
//
// Client configuration (ansible.cfg). ansible-galaxy sends its
// "token" as "Authorization: Token …", which kutu does not accept, so
// pass the kutu token as the basic-auth password instead:
//
//	[galaxy]
//	server_list = kutu
//
//	[galaxy_server.kutu]
//	url = https://kutu.example.com/registries/{ns}/{repo}/api/
//	username = kutu
//	password = <kutu token>
package ansible

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

const typ = service.RegistryTypeAnsible

const (
	collDir    = "collections"
	metaDir    = "meta"
	recordFile = "version.json"
)

var (
	nameRe         = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	collPathRe     = regexp.MustCompile(`^/api/(?:content/[^/]+/)?v3/(?:plugin/ansible/content/[^/]+/collections/index|collections)/`)
	artifactPathRe = regexp.MustCompile(`^/api/(?:content/[^/]+/)?v3/plugin/ansible/content/[^/]+/collections/artifacts/`)
	uploadPathRe   = regexp.MustCompile(`^/api/(?:content/[^/]+/)?v3/(?:plugin/ansible/content/[^/]+/collections/)?artifacts/collections/?$`)
	importPathRe   = regexp.MustCompile(`^/api/(?:content/[^/]+/)?v3/imports/collections/([^/]+)/?$`)
	requiresRe     = regexp.MustCompile(`(?m)^requires_ansible:\s*['"]?([^'"\n]+?)['"]?\s*$`)
)

// versionRecord is the stored metadata of one collection version.
type versionRecord struct {
	Namespace       string         `json:"namespace"`
	Name            string         `json:"name"`
	Version         string         `json:"version"`
	Filename        string         `json:"filename"`
	SHA256          string         `json:"sha256,omitempty"`
	Size            int64          `json:"size,omitempty"`
	CreatedAt       string         `json:"created_at,omitempty"`
	RequiresAnsible string         `json:"requires_ansible,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	// DownloadURL is the upstream artifact URL (remote only).
	DownloadURL string `json:"download_url,omitempty"`
}

// versionEntry is one row of a version list.
type versionEntry struct {
	Version         string `json:"version"`
	CreatedAt       string `json:"created_at,omitempty"`
	RequiresAnsible string `json:"requires_ansible,omitempty"`
}

func filename(ns, name, v string) string { return ns + "-" + name + "-" + v + ".tar.gz" }

func validVersion(v string) bool {
	return v != "" && v != "." && v != ".." && !strings.ContainsAny(v, "/\\\x00")
}

// splitPkg accepts "ns.name" or "ns/name".
func splitPkg(pkg string) (string, string, bool) {
	pkg = strings.Trim(pkg, "/")
	i := strings.IndexAny(pkg, "./")
	if i <= 0 {
		return "", "", false
	}
	ns, name := pkg[:i], pkg[i+1:]
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		return "", "", false
	}
	return ns, name, true
}

// parseFilename splits "{ns}-{name}-{version}.tar.gz".
func parseFilename(file string) (ns, name, v string, ok bool) {
	base, found := strings.CutSuffix(file, ".tar.gz")
	if !found || strings.Contains(base, "/") {
		return "", "", "", false
	}
	parts := strings.SplitN(base, "-", 3)
	if len(parts) != 3 || !nameRe.MatchString(parts[0]) || !nameRe.MatchString(parts[1]) || !validVersion(parts[2]) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// ── routing ──

type route struct {
	kind    string // root | list | collection | versions | version | download | upload | import
	ns      string
	name    string
	version string
	id      string
}

func parseRoute(p string) (route, bool) {
	switch strings.TrimSuffix(p, "/") {
	case "/api", "/api/v3":
		return route{kind: "root"}, true
	}
	if m := importPathRe.FindStringSubmatch(p); m != nil {
		return route{kind: "import", id: m[1]}, true
	}
	if uploadPathRe.MatchString(p) {
		return route{kind: "upload"}, true
	}
	if f, ok := strings.CutPrefix(p, "/download/"); ok {
		return downloadRoute(f)
	}
	if loc := artifactPathRe.FindStringIndex(p); loc != nil {
		return downloadRoute(p[loc[1]:])
	}
	pp := p
	if !strings.HasSuffix(pp, "/") {
		pp += "/"
	}
	loc := collPathRe.FindStringIndex(pp)
	if loc == nil {
		return route{}, false
	}
	rest := strings.Trim(pp[loc[1]:], "/")
	if rest == "" {
		return route{kind: "list"}, true
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || !nameRe.MatchString(parts[0]) || !nameRe.MatchString(parts[1]) {
		return route{}, false
	}
	rt := route{ns: parts[0], name: parts[1]}
	switch {
	case len(parts) == 2:
		rt.kind = "collection"
	case len(parts) == 3 && parts[2] == "versions":
		rt.kind = "versions"
	case len(parts) == 4 && parts[2] == "versions" && validVersion(parts[3]):
		rt.kind, rt.version = "version", parts[3]
	default:
		return route{}, false
	}
	return rt, true
}

func downloadRoute(file string) (route, bool) {
	ns, name, v, ok := parseFilename(file)
	if !ok {
		return route{}, false
	}
	return route{kind: "download", ns: ns, name: name, version: v}, true
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	rt, ok := parseRoute(r.URL.Path)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	switch rt.kind {
	case "download":
		return registry.ArtifactRef{Name: rt.ns + "." + rt.name, Version: rt.version}, true
	case "collection", "versions", "version":
		return registry.ArtifactRef{Name: rt.ns + "." + rt.name}, true
	}
	return registry.ArtifactRef{}, false
}

// ── shared read path ──

// backend is the data source behind the Galaxy wire format; Local,
// Remote and the per-request virtual view implement it.
type backend interface {
	versionList(ctx context.Context, ns, name string) ([]versionEntry, error)
	versionRecord(ctx context.Context, ns, name, v string) (*versionRecord, error)
	serveArtifact(w http.ResponseWriter, r *http.Request, ns, name, v string)
	collections(ctx context.Context) ([]registry.PackageSummary, error)
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

func collHref(prefix, ns, name string) string {
	return prefix + "/api/v3/collections/" + ns + "/" + name + "/"
}

func versionHref(prefix, ns, name, v string) string {
	return collHref(prefix, ns, name) + "versions/" + v + "/"
}

func pageParams(r *http.Request) (int, int) {
	limit, offset := 100, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = n
	}
	return limit, offset
}

func pageLinks(base string, limit, offset, count int) map[string]any {
	link := func(o int) string { return fmt.Sprintf("%s?limit=%d&offset=%d", base, limit, o) }
	last := 0
	if count > 0 {
		last = ((count - 1) / limit) * limit
	}
	links := map[string]any{"first": link(0), "previous": nil, "next": nil, "last": link(last)}
	if offset > 0 {
		links["previous"] = link(max(0, offset-limit))
	}
	if offset+limit < count {
		links["next"] = link(offset + limit)
	}
	return links
}

func page[T any](items []T, limit, offset int) []T {
	if offset >= len(items) {
		return []T{}
	}
	return items[offset:min(len(items), offset+limit)]
}

func serveRead(w http.ResponseWriter, r *http.Request, b backend) {
	rt, ok := parseRoute(r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	ctx := r.Context()
	prefix := pkgbase.Prefix(r)
	switch rt.kind {
	case "root":
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
			"description":        "kutu Ansible Galaxy API",
			"current_version":    "v3",
			"available_versions": map[string]string{"v3": "v3/"},
		})
	case "list":
		pkgs, err := b.collections(ctx)
		if err != nil {
			backendError(w, err)
			return
		}
		limit, offset := pageParams(r)
		data := make([]map[string]any, 0, limit)
		for _, p := range page(pkgs, limit, offset) {
			ns, name, ok := splitPkg(p.Name)
			if !ok {
				continue
			}
			hv := pkgbase.Latest(p.Versions)
			data = append(data, map[string]any{
				"href": collHref(prefix, ns, name), "namespace": ns, "name": name, "deprecated": false,
				"versions_url":    collHref(prefix, ns, name) + "versions/",
				"highest_version": map[string]string{"href": versionHref(prefix, ns, name, hv), "version": hv},
			})
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
			"meta":  map[string]int{"count": len(pkgs)},
			"links": pageLinks(prefix+"/api/v3/collections/", limit, offset, len(pkgs)),
			"data":  data,
		})
	case "collection":
		entries, err := b.versionList(ctx, rt.ns, rt.name)
		if err != nil {
			backendError(w, err)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, renderCollection(prefix, rt.ns, rt.name, entries))
	case "versions":
		entries, err := b.versionList(ctx, rt.ns, rt.name)
		if err != nil {
			backendError(w, err)
			return
		}
		limit, offset := pageParams(r)
		newest := make([]versionEntry, len(entries))
		for i, e := range entries {
			newest[len(entries)-1-i] = e
		}
		data := make([]map[string]any, 0, limit)
		for _, e := range page(newest, limit, offset) {
			data = append(data, map[string]any{
				"version": e.Version, "href": versionHref(prefix, rt.ns, rt.name, e.Version),
				"created_at": e.CreatedAt, "updated_at": e.CreatedAt, "requires_ansible": e.RequiresAnsible, "marks": []any{},
			})
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
			"meta":  map[string]int{"count": len(entries)},
			"links": pageLinks(collHref(prefix, rt.ns, rt.name)+"versions/", limit, offset, len(entries)),
			"data":  data,
		})
	case "version":
		rec, err := b.versionRecord(ctx, rt.ns, rt.name, rt.version)
		if err != nil {
			backendError(w, err)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, renderVersion(r, rec))
	case "download":
		b.serveArtifact(w, r, rt.ns, rt.name, rt.version)
	case "import":
		serveImport(w, r, b, rt.id)
	default:
		pkgbase.NotFound(w)
	}
}

func renderCollection(prefix, ns, name string, entries []versionEntry) map[string]any {
	vs := make([]string, 0, len(entries))
	var created, updated string
	for _, e := range entries {
		vs = append(vs, e.Version)
		if e.CreatedAt != "" && (created == "" || e.CreatedAt < created) {
			created = e.CreatedAt
		}
		if e.CreatedAt > updated {
			updated = e.CreatedAt
		}
	}
	hv := pkgbase.Latest(vs)
	return map[string]any{
		"href": collHref(prefix, ns, name), "namespace": ns, "name": name, "deprecated": false,
		"versions_url":    collHref(prefix, ns, name) + "versions/",
		"highest_version": map[string]string{"href": versionHref(prefix, ns, name, hv), "version": hv},
		"created_at":      created, "updated_at": updated, "download_count": 0,
	}
}

func renderVersion(r *http.Request, rec *versionRecord) map[string]any {
	prefix := pkgbase.Prefix(r)
	meta := rec.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	if meta["dependencies"] == nil {
		meta["dependencies"] = map[string]any{}
	}
	file := filename(rec.Namespace, rec.Name, rec.Version)
	return map[string]any{
		"version": rec.Version, "href": versionHref(prefix, rec.Namespace, rec.Name, rec.Version),
		"created_at": rec.CreatedAt, "updated_at": rec.CreatedAt, "requires_ansible": rec.RequiresAnsible, "marks": []any{},
		"artifact":     map[string]any{"filename": file, "sha256": rec.SHA256, "size": rec.Size},
		"collection":   map[string]any{"id": rec.Namespace + "." + rec.Name, "name": rec.Name, "href": collHref(prefix, rec.Namespace, rec.Name)},
		"download_url": pkgbase.PublicBase(r) + "/download/" + file,
		"name":         rec.Name,
		"namespace":    map[string]any{"name": rec.Namespace, "metadata_sha256": nil},
		"signatures":   []any{},
		"metadata":     meta,
		"git_url":      nil, "git_commit_sha": nil, "manifest": nil, "files": nil,
	}
}

func serveImport(w http.ResponseWriter, r *http.Request, b backend, id string) {
	ns, name, v, ok := parseFilename(id + ".tar.gz")
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	rec, err := b.versionRecord(r.Context(), ns, name, v)
	if err != nil {
		backendError(w, err)
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
		"id": id, "state": "completed", "error": nil, "messages": []any{},
		"created_at": rec.CreatedAt, "updated_at": rec.CreatedAt, "started_at": rec.CreatedAt, "finished_at": rec.CreatedAt,
		"namespace": ns, "name": name, "version": v,
	})
}

// ── tarball parsing ──

func cleanTarName(n string) string {
	return strings.Trim(path.Clean("/"+strings.TrimPrefix(n, "./")), "/")
}

// readTarFiles returns the contents of the regular files in a .tar.gz
// whose cleaned names satisfy want.
func readTarFiles(body []byte, want func(string) bool) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := cleanTarName(hdr.Name)
		if !want(name) {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// parseCollection reads MANIFEST.json (and meta/runtime.yml) from a
// collection tarball.
func parseCollection(body []byte) (*versionRecord, error) {
	files, err := readTarFiles(body, func(n string) bool { return n == "MANIFEST.json" || n == "meta/runtime.yml" })
	if err != nil {
		return nil, err
	}
	raw, ok := files["MANIFEST.json"]
	if !ok {
		return nil, errors.New("MANIFEST.json not found in collection tarball")
	}
	var m struct {
		CollectionInfo map[string]any `json:"collection_info"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse MANIFEST.json: %w", err)
	}
	ci := m.CollectionInfo
	rec := &versionRecord{Namespace: str(ci["namespace"]), Name: str(ci["name"]), Version: str(ci["version"])}
	if !nameRe.MatchString(rec.Namespace) || !nameRe.MatchString(rec.Name) || !validVersion(rec.Version) {
		return nil, errors.New("MANIFEST.json collection_info must carry a valid namespace, name and version")
	}
	rec.Filename = filename(rec.Namespace, rec.Name, rec.Version)
	rec.Metadata = map[string]any{}
	for k, v := range ci {
		switch k {
		case "namespace", "name", "version":
		default:
			rec.Metadata[k] = v
		}
	}
	if rec.Metadata["dependencies"] == nil {
		rec.Metadata["dependencies"] = map[string]any{}
	}
	if rt, ok := files["meta/runtime.yml"]; ok {
		if mm := requiresRe.FindSubmatch(rt); mm != nil {
			rec.RequiresAnsible = strings.TrimSpace(string(mm[1]))
		}
	}
	return rec, nil
}

// ── Store ──

// Store wraps pkgbase.Store with the collection layout:
// collections/{ns}/{name}/{version}/{version.json,<tarball>}.
type Store struct{ *pkgbase.Store }

func (s *Store) versionDir(ns, name, v string) string { return path.Join(collDir, ns, name, v) }
func (s *Store) recordRel(ns, name, v string) string {
	return path.Join(s.versionDir(ns, name, v), recordFile)
}
func (s *Store) artifactRel(ns, name, v string) string {
	return path.Join(s.versionDir(ns, name, v), filename(ns, name, v))
}

// Record returns the stored metadata of ns.name@v.
func (s *Store) Record(ns, name, v string) (*versionRecord, error) {
	b, err := s.Read(s.recordRel(ns, name, v))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, registry.ErrPackageNotFound
		}
		return nil, err
	}
	var rec versionRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Versions returns the stored versions of ns.name (ascending).
func (s *Store) Versions(ns, name string) ([]string, error) {
	dirs, err := s.ListDirs(path.Join(collDir, ns, name))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if s.Exists(s.recordRel(ns, name, d)) {
			out = append(out, d)
		}
	}
	pkgbase.SortVersions(out)
	return out, nil
}

func (s *Store) entries(ns, name string) ([]versionEntry, error) {
	vs, err := s.Versions(ns, name)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	out := make([]versionEntry, 0, len(vs))
	for _, v := range vs {
		e := versionEntry{Version: v}
		if rec, err := s.Record(ns, name, v); err == nil {
			e.CreatedAt, e.RequiresAnsible = rec.CreatedAt, rec.RequiresAnsible
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Store) put(rec *versionRecord, tarball []byte) error {
	if tarball != nil {
		if err := s.Write(s.artifactRel(rec.Namespace, rec.Name, rec.Version), tarball); err != nil {
			return err
		}
	}
	b, err := pkgbase.MarshalJSON(rec)
	if err != nil {
		return err
	}
	return s.Write(s.recordRel(rec.Namespace, rec.Name, rec.Version), b)
}

// ListPackages lists every stored collection as "ns.name".
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	nss, err := s.ListDirs(collDir)
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, ns := range nss {
		names, _ := s.ListDirs(path.Join(collDir, ns))
		for _, name := range names {
			vs, _ := s.Versions(ns, name)
			if len(vs) > 0 {
				out = append(out, registry.PackageSummary{Name: ns + "." + name, Versions: vs})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteVersion removes pkg ("ns.name") @ version.
func (s *Store) DeleteVersion(_ context.Context, pkg, version string) error {
	ns, name, ok := splitPkg(pkg)
	if !ok || !validVersion(version) || !s.Exists(s.recordRel(ns, name, version)) {
		return registry.ErrPackageNotFound
	}
	if _, _, errs := s.DeleteTree(s.versionDir(ns, name, version)); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func licenseOf(meta map[string]any) string {
	switch l := meta["license"].(type) {
	case string:
		return l
	case []any:
		var parts []string
		for _, x := range l {
			if s := str(x); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " OR ")
	}
	return ""
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	ns, name, ok := splitPkg(ref.Name)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	rec, err := s.Record(ns, name, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	return registry.ArtifactMeta{License: licenseOf(rec.Metadata), PublishedAt: parseTime(rec.CreatedAt)}, nil
}

func detail(s *Store, pkg string) (*registry.PackageDetail, error) {
	ns, name, ok := splitPkg(pkg)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	vs, err := s.Versions(ns, name)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d := &registry.GenericPackageDetail{LatestVersion: pkgbase.Latest(vs), Metadata: map[string]string{"namespace": ns}}
	for i := len(vs) - 1; i >= 0; i-- {
		rec, err := s.Record(ns, name, vs[i])
		if err != nil {
			continue
		}
		row := registry.GenericVersionDetail{Version: rec.Version, Size: rec.Size, Metadata: map[string]string{}}
		if t := parseTime(rec.CreatedAt); !t.IsZero() {
			row.PublishedAt = t.UTC().Format(time.RFC3339)
		}
		if fi, err := s.Stat(s.artifactRel(ns, name, rec.Version)); err == nil {
			row.Files = []registry.GenericFile{{Name: filename(ns, name, rec.Version), Size: fi.Size, SHA256: rec.SHA256}}
		}
		if deps, ok := rec.Metadata["dependencies"].(map[string]any); ok && len(deps) > 0 {
			keys := make([]string, 0, len(deps))
			for k, v := range deps {
				keys = append(keys, k+" "+str(v))
			}
			sort.Strings(keys)
			row.Metadata["dependencies"] = strings.Join(keys, ", ")
		}
		if rec.RequiresAnsible != "" {
			row.Metadata["requires_ansible"] = rec.RequiresAnsible
		}
		if rec.Version == d.LatestVersion {
			d.Description = str(rec.Metadata["description"])
			d.Homepage = str(rec.Metadata["homepage"])
			d.License = licenseOf(rec.Metadata)
			if repo := str(rec.Metadata["repository"]); repo != "" {
				d.Metadata["repository"] = repo
			}
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: ns + "." + name, Generic: d}, nil
}

// ── Local ──

// Local is a hosted Galaxy collection repository.
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

func (l *Local) versionList(_ context.Context, ns, name string) ([]versionEntry, error) {
	return l.store.entries(ns, name)
}

func (l *Local) versionRecord(_ context.Context, ns, name, v string) (*versionRecord, error) {
	return l.store.Record(ns, name, v)
}

func (l *Local) serveArtifact(w http.ResponseWriter, r *http.Request, ns, name, v string) {
	if !pkgbase.ServeStored(w, r, l.store.Store, l.store.artifactRel(ns, name, v), "application/gzip") {
		pkgbase.NotFound(w)
	}
}

func (l *Local) collections(ctx context.Context) ([]registry.PackageSummary, error) {
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

// PromoteVersion copies name@version into dst (a local ansible repo).
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("ansible: promote target %s/%s is not a local ansible repository", dst.Namespace(), dst.Name())
	}
	ns, coll, ok := splitPkg(name)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	rec, err := l.store.Record(ns, coll, version)
	if err != nil {
		return err
	}
	body, err := l.store.Read(l.store.artifactRel(ns, coll, version))
	if err != nil {
		return err
	}
	if _, err := d.Guard.Check(d.store.Store, d.store.Exists(d.store.recordRel(ns, coll, version)), int64(len(body))); err != nil {
		return err
	}
	if err := d.store.put(rec, body); err != nil {
		return err
	}
	d.EmitPublished(ns+"."+coll+"@"+version, int64(len(body)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveRead(w, r, l)
	case http.MethodPost, http.MethodPut:
		if rt, ok := parseRoute(r.URL.Path); ok && rt.kind == "upload" {
			l.publish(w, r)
			return
		}
		pkgbase.NotFound(w)
	case http.MethodDelete:
		l.remove(w, r)
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
	sum := pkgbase.SHA256Hex(body)
	if want := r.FormValue("sha256"); want != "" && !strings.EqualFold(want, sum) {
		pkgbase.Error(w, http.StatusBadRequest, "sha256 mismatch")
		return
	}
	rec, err := parseCollection(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if !l.AllowPublish(w, l.store.Exists(l.store.recordRel(rec.Namespace, rec.Name, rec.Version)), int64(len(body))) {
		return
	}
	rec.SHA256, rec.Size = sum, int64(len(body))
	rec.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := l.store.put(rec, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(rec.Namespace+"."+rec.Name+"@"+rec.Version, rec.Size)
	id := rec.Namespace + "-" + rec.Name + "-" + rec.Version
	pkgbase.WriteJSON(w, r, http.StatusAccepted, map[string]string{
		"task": pkgbase.PublicBase(r) + "/api/v3/imports/collections/" + id + "/",
	})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	rt, ok := parseRoute(r.URL.Path)
	if !ok || (rt.kind != "version" && rt.kind != "collection") {
		pkgbase.NotFound(w)
		return
	}
	pkg := rt.ns + "." + rt.name
	versions := []string{rt.version}
	if rt.kind == "collection" {
		versions, _ = l.store.Versions(rt.ns, rt.name)
		if len(versions) == 0 {
			pkgbase.NotFound(w)
			return
		}
	}
	for _, v := range versions {
		if err := l.store.DeleteVersion(r.Context(), pkg, v); err != nil {
			backendError(w, err)
			return
		}
		l.EmitDeleted(pkg + "@" + v)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote ──

// Remote is a pull-through proxy of a Galaxy server.
type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/api/")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func upstreamPaths(ns, name, suffix string) []string {
	return []string{
		"/api/v3/plugin/ansible/content/published/collections/index/" + ns + "/" + name + "/" + suffix,
		"/api/v3/collections/" + ns + "/" + name + "/" + suffix,
	}
}

// absURL resolves an upstream href (absolute, root-relative or
// relative) against the upstream base URL.
func (rr *Remote) absURL(ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	base, err := url.Parse(rr.Client.BaseURL() + "/")
	if err != nil {
		return ref
	}
	u, err := base.Parse(ref)
	if err != nil {
		return ref
	}
	return u.String()
}

func (rr *Remote) get(ctx context.Context, p string) ([]byte, error) {
	resp, err := rr.Client.Get(ctx, p)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (rr *Remote) getFirst(ctx context.Context, paths []string) ([]byte, error) {
	var err error
	for _, p := range paths {
		var b []byte
		if b, err = rr.get(ctx, p); err == nil {
			return b, nil
		}
		if !errors.Is(err, upstream.ErrNotFound) {
			return nil, err
		}
	}
	return nil, err
}

func (rr *Remote) fetchVersions(ctx context.Context, ns, name string) ([]versionEntry, error) {
	body, err := rr.getFirst(ctx, upstreamPaths(ns, name, "versions/?limit=100"))
	if err != nil {
		return nil, err
	}
	var out []versionEntry
	for i := 0; i < 500; i++ {
		var pg struct {
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
			Data []versionEntry `json:"data"`
		}
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, fmt.Errorf("parse upstream version list: %w", err)
		}
		out = append(out, pg.Data...)
		if pg.Links.Next == "" {
			break
		}
		if body, err = rr.get(ctx, rr.absURL(pg.Links.Next)); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return pkgbase.CompareVersions(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

func (rr *Remote) versionList(ctx context.Context, ns, name string) ([]versionEntry, error) {
	cache := path.Join(metaDir, ns, name, "versions.json")
	readCache := func() ([]versionEntry, error) {
		b, err := rr.store.Read(cache)
		if err != nil {
			return nil, err
		}
		var out []versionEntry
		return out, json.Unmarshal(b, &out)
	}
	if rr.Fresh(cache) {
		if out, err := readCache(); err == nil {
			return out, nil
		}
	}
	out, err := rr.fetchVersions(ctx, ns, name)
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

func (rr *Remote) versionRecord(ctx context.Context, ns, name, v string) (*versionRecord, error) {
	if rec, err := rr.store.Record(ns, name, v); err == nil {
		return rec, nil
	}
	body, err := rr.getFirst(ctx, upstreamPaths(ns, name, "versions/"+v+"/"))
	if err != nil {
		return nil, err
	}
	var uv struct {
		Version         string `json:"version"`
		CreatedAt       string `json:"created_at"`
		RequiresAnsible string `json:"requires_ansible"`
		Artifact        struct {
			Filename string `json:"filename"`
			SHA256   string `json:"sha256"`
			Size     int64  `json:"size"`
		} `json:"artifact"`
		DownloadURL string         `json:"download_url"`
		Metadata    map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(body, &uv); err != nil {
		return nil, fmt.Errorf("parse upstream version: %w", err)
	}
	if uv.Version == "" {
		uv.Version = v
	}
	rec := &versionRecord{
		Namespace: ns, Name: name, Version: uv.Version, Filename: filename(ns, name, uv.Version),
		SHA256: uv.Artifact.SHA256, Size: uv.Artifact.Size, CreatedAt: uv.CreatedAt,
		RequiresAnsible: uv.RequiresAnsible, Metadata: uv.Metadata,
	}
	if uv.DownloadURL != "" {
		rec.DownloadURL = rr.absURL(uv.DownloadURL)
	} else {
		rec.DownloadURL = rr.absURL("/download/" + rec.Filename)
	}
	_ = rr.store.put(rec, nil)
	return rec, nil
}

func (rr *Remote) fetchArtifact(ctx context.Context, rec *versionRecord) ([]byte, error) {
	body, err := rr.get(ctx, rec.DownloadURL)
	if err != nil {
		return nil, err
	}
	if rec.SHA256 != "" && !strings.EqualFold(rec.SHA256, pkgbase.SHA256Hex(body)) {
		return nil, fmt.Errorf("upstream artifact sha256 mismatch for %s: %w", rec.Filename, upstream.ErrTransient)
	}
	_ = rr.store.Write(rr.store.artifactRel(rec.Namespace, rec.Name, rec.Version), body)
	return body, nil
}

func (rr *Remote) serveArtifact(w http.ResponseWriter, r *http.Request, ns, name, v string) {
	if pkgbase.ServeStored(w, r, rr.store.Store, rr.store.artifactRel(ns, name, v), "application/gzip") {
		return
	}
	rec, err := rr.versionRecord(r.Context(), ns, name, v)
	if err != nil {
		backendError(w, err)
		return
	}
	body, err := rr.fetchArtifact(r.Context(), rec)
	if err != nil {
		backendError(w, err)
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, "application/gzip", body)
}

func (rr *Remote) collections(ctx context.Context) ([]registry.PackageSummary, error) {
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
	ns, name, ok := splitPkg(ref.Name)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	if _, err := rr.versionRecord(ctx, ns, name, ref.Version); err != nil {
		return registry.ArtifactMeta{}, err
	}
	return artifactInfo(rr.store, ref)
}

// Prefetch warms the version list, version metadata and tarball.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	ns, coll, ok := splitPkg(name)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	if version == "" {
		entries, err := rr.versionList(ctx, ns, coll)
		if err != nil {
			return err
		}
		vs := make([]string, 0, len(entries))
		for _, e := range entries {
			vs = append(vs, e.Version)
		}
		if version = pkgbase.Latest(vs); version == "" {
			return nil
		}
	}
	rec, err := rr.versionRecord(ctx, ns, coll, version)
	if err != nil {
		return err
	}
	if rr.store.Exists(rr.store.artifactRel(ns, coll, version)) {
		return nil
	}
	_, err = rr.fetchArtifact(ctx, rec)
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

// Virtual merges version lists across members; version metadata and
// tarballs are served first-hit.
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

func (vv *virtualView) versionList(ctx context.Context, ns, name string) ([]versionEntry, error) {
	seen := map[string]bool{}
	var out []versionEntry
	vv.members(func(b backend) bool {
		entries, err := b.versionList(ctx, ns, name)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if !seen[e.Version] {
				seen[e.Version] = true
				out = append(out, e)
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

func (vv *virtualView) versionRecord(ctx context.Context, ns, name, ver string) (*versionRecord, error) {
	var found *versionRecord
	vv.members(func(b backend) bool {
		rec, err := b.versionRecord(ctx, ns, name, ver)
		if err == nil {
			found = rec
			return true
		}
		return false
	})
	if found == nil {
		return nil, registry.ErrPackageNotFound
	}
	return found, nil
}

func (vv *virtualView) serveArtifact(w http.ResponseWriter, r *http.Request, _, _, _ string) {
	if !vv.v.ServeFirstHit(w, r) {
		pkgbase.NotFound(w)
	}
}

func (vv *virtualView) collections(ctx context.Context) ([]registry.PackageSummary, error) {
	return vv.v.ListPackages(ctx)
}
