// Package pub implements a Dart/Flutter hosted pub repository
// (repository spec v2):
//
//	GET    /api/packages/{pkg}                          version listing (application/vnd.pub.v2+json)
//	GET    /api/packages/{pkg}/versions/{v}             one version (deprecated endpoint)
//	GET    /packages/{pkg}/versions/{v}.tar.gz          archive download
//	GET    /api/packages/{pkg}/versions/{v}/archive.tar.gz  archive download (alias)
//	GET    /api/packages/versions/new                   start publish → {url, fields}
//	POST   /api/packages/versions/newUpload             multipart "file" upload → 204 + Location
//	GET    /api/packages/versions/newUploadFinish?id=   finalize publish
//	GET    /api/search?q=                               name search
//	POST   /api/packages/{pkg}/versions/{v}/retract     retract a version (kutu extension)
//	POST   /api/packages/{pkg}/versions/{v}/unretract   undo retraction (kutu extension)
//	PUT    /api/packages/{pkg}/options                  {"isDiscontinued":bool,"replacedBy":"x"}
//	DELETE /api/packages/{pkg}/versions/{v}             delete a version (kutu extension)
//
// Remote repos proxy an upstream pub repository (e.g. https://pub.dev),
// rewriting archive_url to point back at kutu; archives are cached
// forever and package documents honour MutableTTL. Virtual repos merge
// the versions arrays of their members.
//
// Client configuration:
//
//	dart pub token add https://kutu.example.com/registries/{ns}/{repo}
//
// then in pubspec.yaml:
//
//	publish_to: https://kutu.example.com/registries/{ns}/{repo}
//	dependencies:
//	  mypkg:
//	    hosted:
//	      name: mypkg
//	      url: https://kutu.example.com/registries/{ns}/{repo}
//	    version: ^1.0.0
package pub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
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
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypePub

const contentType = "application/vnd.pub.v2+json"

var (
	nameRe    = regexp.MustCompile(`^[a-zA-Z0-9_]{1,128}$`)
	versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+\-]{0,127}$`)
	pendingRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ── Wire types ──

type versionEntry struct {
	Version       string          `json:"version"`
	Retracted     bool            `json:"retracted,omitempty"`
	ArchiveURL    string          `json:"archive_url"`
	ArchiveSHA256 string          `json:"archive_sha256,omitempty"`
	Pubspec       json.RawMessage `json:"pubspec"`
	Published     string          `json:"published,omitempty"`
}

type packageDoc struct {
	Name           string         `json:"name"`
	IsDiscontinued bool           `json:"isDiscontinued,omitempty"`
	ReplacedBy     string         `json:"replacedBy,omitempty"`
	Latest         *versionEntry  `json:"latest,omitempty"`
	Versions       []versionEntry `json:"versions"`
}

// versionMeta is the per-version sidecar stored by local repos.
type versionMeta struct {
	Version   string          `json:"version"`
	SHA256    string          `json:"archive_sha256"`
	Size      int64           `json:"size"`
	Pubspec   json.RawMessage `json:"pubspec"`
	Published string          `json:"published,omitempty"`
	Retracted bool            `json:"retracted,omitempty"`
}

type packageOptions struct {
	IsDiscontinued bool   `json:"isDiscontinued"`
	ReplacedBy     string `json:"replacedBy,omitempty"`
}

func isPrerelease(v string) bool {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	return strings.Contains(v, "-")
}

// finish sorts versions ascending and picks latest: the highest
// stable non-retracted version, falling back to the highest overall.
func (d *packageDoc) finish() {
	sort.SliceStable(d.Versions, func(i, j int) bool {
		return pkgbase.CompareVersions(d.Versions[i].Version, d.Versions[j].Version) < 0
	})
	d.Latest = nil
	for i := len(d.Versions) - 1; i >= 0; i-- {
		if v := d.Versions[i]; !v.Retracted && !isPrerelease(v.Version) {
			d.Latest = &d.Versions[i]
			return
		}
	}
	for i := len(d.Versions) - 1; i >= 0; i-- {
		if !d.Versions[i].Retracted {
			d.Latest = &d.Versions[i]
			return
		}
	}
	if n := len(d.Versions); n > 0 {
		d.Latest = &d.Versions[n-1]
	}
}

func (d *packageDoc) find(version string) *versionEntry {
	for i := range d.Versions {
		if d.Versions[i].Version == version {
			return &d.Versions[i]
		}
	}
	return nil
}

func archiveURL(base, pkg, version string) string {
	return base + "/packages/" + url.PathEscape(pkg) + "/versions/" + url.PathEscape(version) + ".tar.gz"
}

// ── HTTP helpers ──

func writeJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	body, err := pkgbase.MarshalJSON(v)
	if err != nil {
		pubError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	pkgbase.WriteBytes(w, r, code, contentType, body)
}

func pubError(w http.ResponseWriter, code int, errCode, msg string) {
	body, _ := pkgbase.MarshalJSON(map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func notFound(w http.ResponseWriter) { pubError(w, http.StatusNotFound, "NotFound", "not found") }

// ── Routing ──

type routeKind int

const (
	rNone routeKind = iota
	rNew
	rUpload
	rFinish
	rPackage
	rOptions
	rVersion
	rRetract
	rUnretract
	rArchive
	rSearch
)

type route struct {
	kind     routeKind
	pkg, ver string
}

func parseRoute(p string) route {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		if u, err := url.PathUnescape(s); err == nil {
			parts[i] = u
		}
	}
	valid := func(rt route) route {
		if !nameRe.MatchString(rt.pkg) || (rt.ver != "" && !versionRe.MatchString(rt.ver)) {
			return route{}
		}
		return rt
	}
	switch {
	case len(parts) == 2 && parts[0] == "api" && parts[1] == "search":
		return route{kind: rSearch}
	case len(parts) == 4 && parts[0] == "packages" && parts[2] == "versions" && strings.HasSuffix(parts[3], ".tar.gz"):
		return valid(route{kind: rArchive, pkg: parts[1], ver: strings.TrimSuffix(parts[3], ".tar.gz")})
	case len(parts) < 3 || parts[0] != "api" || parts[1] != "packages":
		return route{}
	}
	rest := parts[2:]
	if len(rest) == 2 && rest[0] == "versions" {
		switch rest[1] {
		case "new":
			return route{kind: rNew}
		case "newUpload":
			return route{kind: rUpload}
		case "newUploadFinish":
			return route{kind: rFinish}
		}
	}
	switch {
	case len(rest) == 1:
		return valid(route{kind: rPackage, pkg: rest[0]})
	case len(rest) == 2 && rest[1] == "options":
		return valid(route{kind: rOptions, pkg: rest[0]})
	case len(rest) == 3 && rest[1] == "versions":
		return valid(route{kind: rVersion, pkg: rest[0], ver: rest[2]})
	case len(rest) == 4 && rest[1] == "versions":
		switch rest[3] {
		case "retract":
			return valid(route{kind: rRetract, pkg: rest[0], ver: rest[2]})
		case "unretract":
			return valid(route{kind: rUnretract, pkg: rest[0], ver: rest[2]})
		case "archive.tar.gz":
			return valid(route{kind: rArchive, pkg: rest[0], ver: rest[2]})
		}
	}
	return route{}
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	rt := parseRoute(r.URL.Path)
	switch rt.kind {
	case rArchive:
		return registry.ArtifactRef{Name: rt.pkg, Version: rt.ver}, true
	case rPackage, rVersion:
		return registry.ArtifactRef{Name: rt.pkg}, true
	}
	return registry.ArtifactRef{}, false
}

// ── Archive parsing ──

// parsePubspec extracts pubspec.yaml from a .tar.gz archive and
// returns name, version and the pubspec as JSON.
func parsePubspec(archive []byte) (string, string, json.RawMessage, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", "", nil, fmt.Errorf("archive is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", "", nil, errors.New("pubspec.yaml not found in archive")
		}
		if err != nil {
			return "", "", nil, fmt.Errorf("invalid tar archive: %w", err)
		}
		if strings.TrimPrefix(h.Name, "./") != "pubspec.yaml" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return "", "", nil, err
		}
		var spec struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		}
		if err := yaml.Unmarshal(raw, &spec); err != nil {
			return "", "", nil, fmt.Errorf("invalid pubspec.yaml: %w", err)
		}
		if !nameRe.MatchString(spec.Name) {
			return "", "", nil, fmt.Errorf("invalid package name %q", spec.Name)
		}
		if !versionRe.MatchString(spec.Version) {
			return "", "", nil, fmt.Errorf("invalid version %q", spec.Version)
		}
		js, err := yaml.YAMLToJSON(raw)
		if err != nil {
			return "", "", nil, fmt.Errorf("invalid pubspec.yaml: %w", err)
		}
		return spec.Name, spec.Version, json.RawMessage(bytes.TrimSpace(js)), nil
	}
}

// ── Store ──

// Store wraps pkgbase.Store with the pub layout:
//
//	archives/{pkg}/{ver}.tar.gz
//	meta/{pkg}/{ver}.json        version sidecar (local)
//	meta/{pkg}/_options.json     discontinued flag (local)
//	pending/{id}.tar.gz          uploads awaiting finalize
//	upstream/{pkg}.json          cached upstream package document (remote)
type Store struct {
	*pkgbase.Store
	mu *sync.Mutex
}

func wrapStore(s *pkgbase.Store) *Store { return &Store{Store: s, mu: &sync.Mutex{}} }

const optionsFile = "_options.json"

func archiveRel(pkg, ver string) string { return path.Join("archives", pkg, ver+".tar.gz") }
func metaRel(pkg, ver string) string    { return path.Join("meta", pkg, ver+".json") }
func upstreamRel(pkg string) string     { return path.Join("upstream", pkg+".json") }

// Versions returns the stored versions of pkg (ascending).
func (s *Store) Versions(pkg string) []string {
	files, _ := s.ListFiles(path.Join("meta", pkg))
	var out []string
	for _, f := range files {
		if f.Name == optionsFile || !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(f.Name, ".json"))
	}
	pkgbase.SortVersions(out)
	return out
}

func (s *Store) readMeta(pkg, ver string) (*versionMeta, error) {
	b, err := s.Read(metaRel(pkg, ver))
	if err != nil {
		return nil, err
	}
	var m versionMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) writeMeta(pkg string, m *versionMeta) error {
	b, err := pkgbase.MarshalJSON(m)
	if err != nil {
		return err
	}
	return s.Write(metaRel(pkg, m.Version), b)
}

func (s *Store) readOptions(pkg string) packageOptions {
	var o packageOptions
	if b, err := s.Read(path.Join("meta", pkg, optionsFile)); err == nil {
		_ = json.Unmarshal(b, &o)
	}
	return o
}

// ListPackages lists every package with stored versions.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	dirs, err := s.ListDirs("meta")
	if err != nil {
		return nil, err
	}
	out := []registry.PackageSummary{}
	for _, d := range dirs {
		if vs := s.Versions(d); len(vs) > 0 {
			out = append(out, registry.PackageSummary{Name: d, Versions: vs})
		}
	}
	return out, nil
}

// doc builds the package document for pkg. base is the public URL.
func (s *Store) doc(pkg, base string) (*packageDoc, error) {
	vs := s.Versions(pkg)
	if len(vs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	opts := s.readOptions(pkg)
	d := &packageDoc{Name: pkg, IsDiscontinued: opts.IsDiscontinued, ReplacedBy: opts.ReplacedBy}
	for _, v := range vs {
		m, err := s.readMeta(pkg, v)
		if err != nil {
			continue
		}
		d.Versions = append(d.Versions, versionEntry{
			Version: m.Version, Retracted: m.Retracted, ArchiveURL: archiveURL(base, pkg, m.Version),
			ArchiveSHA256: m.SHA256, Pubspec: m.Pubspec, Published: m.Published,
		})
	}
	if len(d.Versions) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d.finish()
	return d, nil
}

// DeleteVersion removes pkg@version.
func (s *Store) DeleteVersion(_ context.Context, pkg, version string) error {
	if !nameRe.MatchString(pkg) || !versionRe.MatchString(version) || !s.Exists(metaRel(pkg, version)) {
		return registry.ErrPackageNotFound
	}
	if err := s.Delete(archiveRel(pkg, version)); err != nil {
		return err
	}
	return s.Delete(metaRel(pkg, version))
}

// ── Detail ──

func pubspecString(spec json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(spec, &m) != nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func detailFromDoc(d *packageDoc, sizeOf func(ver string) int64) *registry.PackageDetail {
	g := &registry.GenericPackageDetail{Metadata: map[string]string{}}
	if d.Latest != nil {
		g.LatestVersion = d.Latest.Version
		g.Description = pubspecString(d.Latest.Pubspec, "description")
		g.Homepage = pubspecString(d.Latest.Pubspec, "homepage")
		if repo := pubspecString(d.Latest.Pubspec, "repository"); repo != "" {
			g.Metadata["repository"] = repo
			if g.Homepage == "" {
				g.Homepage = repo
			}
		}
	}
	if d.IsDiscontinued {
		g.Metadata["discontinued"] = "true"
		if d.ReplacedBy != "" {
			g.Metadata["replaced_by"] = d.ReplacedBy
		}
	}
	if len(g.Metadata) == 0 {
		g.Metadata = nil
	}
	for i := len(d.Versions) - 1; i >= 0; i-- {
		v := d.Versions[i]
		row := registry.GenericVersionDetail{Version: v.Version, PublishedAt: v.Published, Yanked: v.Retracted}
		if sizeOf != nil {
			row.Size = sizeOf(v.Version)
		}
		row.Files = []registry.GenericFile{{Name: d.Name + "-" + v.Version + ".tar.gz", Size: row.Size, SHA256: v.ArchiveSHA256}}
		if sdk := sdkConstraint(v.Pubspec); sdk != "" {
			row.Metadata = map[string]string{"sdk": sdk}
		}
		g.Versions = append(g.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: d.Name, Generic: g}
}

func sdkConstraint(spec json.RawMessage) string {
	var m struct {
		Environment map[string]any `json:"environment"`
	}
	if json.Unmarshal(spec, &m) != nil {
		return ""
	}
	s, _ := m.Environment["sdk"].(string)
	return s
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func search(w http.ResponseWriter, r *http.Request, pkgs []registry.PackageSummary) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	type hit struct {
		Package string `json:"package"`
	}
	out := []hit{}
	for _, p := range pkgs {
		if q == "" || strings.Contains(strings.ToLower(p.Name), q) {
			out = append(out, hit{Package: p.Name})
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"packages": out})
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
		return &Local{Repo: base, store: wrapStore(base.Store)}, nil
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
	if !nameRe.MatchString(name) {
		return nil, registry.ErrInvalidPackageName
	}
	d, err := l.store.doc(name, "")
	if err != nil {
		return nil, err
	}
	return detailFromDoc(d, func(v string) int64 {
		if m, err := l.store.readMeta(name, v); err == nil {
			return m.Size
		}
		return 0
	}), nil
}

func (l *Local) DeleteVersion(ctx context.Context, name, version string) error {
	if err := l.store.DeleteVersion(ctx, name, version); err != nil {
		return err
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if !nameRe.MatchString(ref.Name) || !versionRe.MatchString(ref.Version) {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	m, err := l.store.readMeta(ref.Name, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	return registry.ArtifactMeta{PublishedAt: parseTime(m.Published)}, nil
}

func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("pub: promote target %T is not a pub local registry", dst)
	}
	if !nameRe.MatchString(name) || !versionRe.MatchString(version) {
		return registry.ErrPackageNotFound
	}
	m, err := l.store.readMeta(name, version)
	if err != nil {
		return registry.ErrPackageNotFound
	}
	body, err := l.store.Read(archiveRel(name, version))
	if err != nil {
		return err
	}
	if err := d.store.Write(archiveRel(name, version), body); err != nil {
		return err
	}
	if err := d.store.writeMeta(name, m); err != nil {
		return err
	}
	d.EmitPublished(name+"@"+version, int64(len(body)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := parseRoute(r.URL.Path)
	base := pkgbase.PublicBase(r)
	switch {
	case rt.kind == rSearch && pkgbase.IsRead(r):
		pkgs, _ := l.store.ListPackages(r.Context())
		search(w, r, pkgs)
	case rt.kind == rPackage && pkgbase.IsRead(r):
		d, err := l.store.doc(rt.pkg, base)
		if err != nil {
			notFound(w)
			return
		}
		writeJSON(w, r, http.StatusOK, d)
	case rt.kind == rVersion && pkgbase.IsRead(r):
		d, err := l.store.doc(rt.pkg, base)
		if err != nil {
			notFound(w)
			return
		}
		v := d.find(rt.ver)
		if v == nil {
			notFound(w)
			return
		}
		writeJSON(w, r, http.StatusOK, v)
	case rt.kind == rVersion && r.Method == http.MethodDelete:
		if !l.checkPush(w) {
			return
		}
		if err := l.DeleteVersion(r.Context(), rt.pkg, rt.ver); err != nil {
			if pkgbase.IsNotFound(err) {
				notFound(w)
				return
			}
			pubError(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"success": map[string]string{"message": "deleted " + rt.pkg + " " + rt.ver}})
	case rt.kind == rArchive && pkgbase.IsRead(r):
		if !pkgbase.ServeStored(w, r, l.store.Store, archiveRel(rt.pkg, rt.ver), "application/octet-stream") {
			notFound(w)
		}
	case rt.kind == rNew && pkgbase.IsRead(r):
		if !l.checkPush(w) {
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"url": base + "/api/packages/versions/newUpload", "fields": map[string]string{}})
	case rt.kind == rUpload && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		l.upload(w, r, base)
	case rt.kind == rFinish && pkgbase.IsRead(r):
		l.finish(w, r)
	case (rt.kind == rRetract || rt.kind == rUnretract) && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		l.retract(w, r, rt)
	case rt.kind == rOptions && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		l.options(w, r, rt.pkg)
	case rt.kind == rNone:
		notFound(w)
	default:
		pubError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed")
	}
}

func (l *Local) checkPush(w http.ResponseWriter) bool {
	if !l.AllowPush {
		pubError(w, http.StatusMethodNotAllowed, "PushDisabled", "push disabled for this repository")
		return false
	}
	return true
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request, base string) {
	if !l.checkPush(w) {
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		pubError(w, http.StatusBadRequest, "InvalidInput", "expected multipart/form-data upload")
		return
	}
	var archive []byte
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			pubError(w, http.StatusBadRequest, "InvalidInput", err.Error())
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		archive, err = pkgbase.ReadBody(part, l.MaxUpload)
		_ = part.Close()
		if err != nil {
			pubError(w, http.StatusRequestEntityTooLarge, "PackageTooLarge", err.Error())
			return
		}
		break
	}
	if archive == nil {
		pubError(w, http.StatusBadRequest, "InvalidInput", `missing multipart field "file"`)
		return
	}
	if _, _, _, err := parsePubspec(archive); err != nil {
		pubError(w, http.StatusBadRequest, "InvalidInput", err.Error())
		return
	}
	var idb [16]byte
	_, _ = rand.Read(idb[:])
	id := hex.EncodeToString(idb[:])
	if err := l.store.Write(path.Join("pending", id+".tar.gz"), archive); err != nil {
		pubError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("Location", base+"/api/packages/versions/newUploadFinish?id="+id)
	w.WriteHeader(http.StatusNoContent)
}

func (l *Local) finish(w http.ResponseWriter, r *http.Request) {
	if !l.checkPush(w) {
		return
	}
	id := r.URL.Query().Get("id")
	if !pendingRe.MatchString(id) {
		pubError(w, http.StatusBadRequest, "InvalidInput", "invalid upload id")
		return
	}
	pending := path.Join("pending", id+".tar.gz")
	archive, err := l.store.Read(pending)
	if err != nil {
		pubError(w, http.StatusNotFound, "NotFound", "upload not found or already finalized")
		return
	}
	defer func() { _ = l.store.Delete(pending) }()
	name, version, spec, err := parsePubspec(archive)
	if err != nil {
		pubError(w, http.StatusBadRequest, "InvalidInput", err.Error())
		return
	}
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	exists := l.store.Exists(metaRel(name, version))
	if code, err := l.Guard.Check(l.store.Store, exists, int64(len(archive))); err != nil {
		pubError(w, code, "PolicyViolation", err.Error())
		return
	}
	if err := l.store.Write(archiveRel(name, version), archive); err != nil {
		pubError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	m := &versionMeta{
		Version: version, SHA256: pkgbase.SHA256Hex(archive), Size: int64(len(archive)),
		Pubspec: spec, Published: time.Now().UTC().Format(time.RFC3339),
	}
	if err := l.store.writeMeta(name, m); err != nil {
		pubError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	l.EmitPublished(name+"@"+version, int64(len(archive)))
	writeJSON(w, r, http.StatusOK, map[string]any{"success": map[string]string{"message": "Successfully uploaded " + name + " version " + version + "."}})
}

func (l *Local) retract(w http.ResponseWriter, r *http.Request, rt route) {
	if !l.checkPush(w) {
		return
	}
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	m, err := l.store.readMeta(rt.pkg, rt.ver)
	if err != nil {
		notFound(w)
		return
	}
	m.Retracted = rt.kind == rRetract
	if err := l.store.writeMeta(rt.pkg, m); err != nil {
		pubError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"success": map[string]string{"message": "updated " + rt.pkg + " " + rt.ver}})
}

func (l *Local) options(w http.ResponseWriter, r *http.Request, pkg string) {
	if !l.checkPush(w) {
		return
	}
	if len(l.store.Versions(pkg)) == 0 {
		notFound(w)
		return
	}
	body, err := pkgbase.ReadBody(r.Body, 1<<16)
	if err != nil {
		pubError(w, http.StatusRequestEntityTooLarge, "InvalidInput", err.Error())
		return
	}
	var o packageOptions
	if err := json.Unmarshal(body, &o); err != nil {
		pubError(w, http.StatusBadRequest, "InvalidInput", "invalid JSON body")
		return
	}
	b, _ := pkgbase.MarshalJSON(o)
	if err := l.store.Write(path.Join("meta", pkg, optionsFile), b); err != nil {
		pubError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, o)
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/api/packages/meta")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: wrapStore(base.Store)}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

// upstreamDoc returns the (cached) upstream package document.
func (rr *Remote) upstreamDoc(ctx context.Context, pkg string) (*packageDoc, error) {
	body, err := rr.FetchCached(ctx, upstreamRel(pkg), "/api/packages/"+url.PathEscape(pkg), true)
	if err != nil {
		return nil, err
	}
	return decodeDoc(body)
}

func decodeDoc(body []byte) (*packageDoc, error) {
	var d packageDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("pub: invalid package document: %w", err)
	}
	return &d, nil
}

// rewrite points every archive_url back at kutu.
func rewrite(d *packageDoc, base string) *packageDoc {
	out := &packageDoc{Name: d.Name, IsDiscontinued: d.IsDiscontinued, ReplacedBy: d.ReplacedBy}
	for _, v := range d.Versions {
		v.ArchiveURL = archiveURL(base, d.Name, v.Version)
		if len(v.Pubspec) == 0 {
			v.Pubspec = json.RawMessage("{}")
		}
		out.Versions = append(out.Versions, v)
	}
	out.finish()
	return out
}

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	dirs, err := rr.store.ListDirs("archives")
	if err != nil {
		return nil, err
	}
	out := []registry.PackageSummary{}
	for _, d := range dirs {
		files, _ := rr.store.ListFiles(path.Join("archives", d))
		var vs []string
		for _, f := range files {
			if strings.HasSuffix(f.Name, ".tar.gz") {
				vs = append(vs, strings.TrimSuffix(f.Name, ".tar.gz"))
			}
		}
		if len(vs) > 0 {
			pkgbase.SortVersions(vs)
			out = append(out, registry.PackageSummary{Name: d, Versions: vs})
		}
	}
	return out, nil
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	if !nameRe.MatchString(name) {
		return nil, registry.ErrInvalidPackageName
	}
	body, err := rr.store.Read(upstreamRel(name))
	if err != nil {
		return nil, registry.ErrPackageNotFound
	}
	d, err := decodeDoc(body)
	if err != nil {
		return nil, err
	}
	d = rewrite(d, "")
	return detailFromDoc(d, func(v string) int64 {
		if fi, err := rr.store.Stat(archiveRel(name, v)); err == nil {
			return fi.Size
		}
		return 0
	}), nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, "upstream", "search"), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if !nameRe.MatchString(ref.Name) {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	d, err := rr.upstreamDoc(ctx, ref.Name)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	v := d.find(ref.Version)
	if v == nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	return registry.ArtifactMeta{PublishedAt: parseTime(v.Published)}, nil
}

// fetchArchive returns pkg@ver's archive, caching it on first fetch.
func (rr *Remote) fetchArchive(ctx context.Context, pkg, ver string) ([]byte, error) {
	rel := archiveRel(pkg, ver)
	if rr.store.Exists(rel) {
		return rr.store.Read(rel)
	}
	src := "/packages/" + url.PathEscape(pkg) + "/versions/" + url.PathEscape(ver) + ".tar.gz"
	var want string
	if d, err := rr.upstreamDoc(ctx, pkg); err == nil {
		if v := d.find(ver); v != nil {
			if v.ArchiveURL != "" {
				src = v.ArchiveURL
			}
			want = strings.ToLower(v.ArchiveSHA256)
		}
	}
	body, err := rr.FetchCached(ctx, rel, src, false)
	if err != nil {
		return nil, err
	}
	if want != "" && pkgbase.SHA256Hex(body) != want {
		_ = rr.store.Delete(rel)
		return nil, fmt.Errorf("pub: archive checksum mismatch for %s %s", pkg, ver)
	}
	return body, nil
}

func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if !nameRe.MatchString(name) {
		return registry.ErrInvalidPackageName
	}
	d, err := rr.upstreamDoc(ctx, name)
	if err != nil {
		return err
	}
	if version == "" {
		d = rewrite(d, "")
		if d.Latest == nil {
			return registry.ErrPackageNotFound
		}
		version = d.Latest.Version
	}
	_, err = rr.fetchArchive(ctx, name, version)
	return err
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pubError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "remote registry is read-only")
		return
	}
	rt := parseRoute(r.URL.Path)
	base := pkgbase.PublicBase(r)
	switch rt.kind {
	case rPackage, rVersion:
		d, err := rr.upstreamDoc(r.Context(), rt.pkg)
		if err != nil {
			upstreamErr(w, err)
			return
		}
		d = rewrite(d, base)
		if rt.kind == rPackage {
			writeJSON(w, r, http.StatusOK, d)
			return
		}
		if v := d.find(rt.ver); v != nil {
			writeJSON(w, r, http.StatusOK, v)
			return
		}
		notFound(w)
	case rArchive:
		if pkgbase.ServeStored(w, r, rr.store.Store, archiveRel(rt.pkg, rt.ver), "application/octet-stream") {
			return
		}
		body, err := rr.fetchArchive(r.Context(), rt.pkg, rt.ver)
		if err != nil {
			upstreamErr(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/octet-stream", body)
	case rSearch:
		q := r.URL.Query().Get("q")
		key := path.Join("search", pkgbase.SHA256Hex([]byte(q))+".json")
		body, err := rr.FetchCached(r.Context(), key, "/api/search?q="+url.QueryEscape(q), true)
		if err != nil {
			upstreamErr(w, err)
			return
		}
		// Drop upstream pagination links; they point at the upstream host.
		var res struct {
			Packages []json.RawMessage `json:"packages"`
		}
		if json.Unmarshal(body, &res) != nil {
			pubError(w, http.StatusBadGateway, "UpstreamError", "invalid upstream search response")
			return
		}
		if res.Packages == nil {
			res.Packages = []json.RawMessage{}
		}
		writeJSON(w, r, http.StatusOK, res)
	case rNew, rUpload, rFinish:
		pubError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "remote registry is read-only")
	default:
		notFound(w)
	}
}

func upstreamErr(w http.ResponseWriter, err error) {
	if pkgbase.IsNotFound(err) {
		notFound(w)
		return
	}
	pubError(w, http.StatusBadGateway, "UpstreamError", err.Error())
}

// ── Virtual ──

// Virtual merges package documents and search results across members.
type Virtual struct {
	*pkgbase.Virtual
}

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
	rt := parseRoute(r.URL.Path)
	if !pkgbase.IsRead(r) || rt.kind == rNew || rt.kind == rFinish {
		pubError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "virtual repositories are read-only; publish to a local member")
		return
	}
	switch rt.kind {
	case rPackage:
		merged := &packageDoc{Name: rt.pkg}
		seen := map[string]bool{}
		for _, body := range v.CollectMembers(r) {
			d, err := decodeDoc(body)
			if err != nil {
				continue
			}
			merged.IsDiscontinued = merged.IsDiscontinued || d.IsDiscontinued
			if merged.ReplacedBy == "" {
				merged.ReplacedBy = d.ReplacedBy
			}
			for _, ver := range d.Versions {
				if !seen[ver.Version] {
					seen[ver.Version] = true
					merged.Versions = append(merged.Versions, ver)
				}
			}
		}
		if len(merged.Versions) == 0 {
			notFound(w)
			return
		}
		merged.finish()
		writeJSON(w, r, http.StatusOK, merged)
	case rSearch:
		type hit struct {
			Package string `json:"package"`
		}
		out := []hit{}
		seen := map[string]bool{}
		for _, body := range v.CollectMembers(r) {
			var res struct {
				Packages []hit `json:"packages"`
			}
			if json.Unmarshal(body, &res) != nil {
				continue
			}
			for _, h := range res.Packages {
				if !seen[h.Package] {
					seen[h.Package] = true
					out = append(out, h)
				}
			}
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"packages": out})
	default:
		if !v.ServeFirstHit(w, r) {
			notFound(w)
		}
	}
}
