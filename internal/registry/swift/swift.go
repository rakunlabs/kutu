// Package swift implements a Swift Package Registry (SE-0292):
//
//	GET    /{scope}/{name}                              list releases (+ Link latest-version)
//	GET    /{scope}/{name}/{version}                    release info (resources, metadata, publishedAt)
//	GET    /{scope}/{name}/{version}/Package.swift      manifest (?swift-version= → Package@swift-X.swift or 303)
//	GET    /{scope}/{name}/{version}.zip                source archive (Digest: sha-256=…)
//	GET    /identifiers?url=                            lookup identifiers by metadata.repositoryURLs
//	PUT    /{scope}/{name}/{version}                    publish (multipart: source-archive, metadata, signatures)
//	DELETE /{scope}/{name}/{version}                    delete a release (kutu extension)
//	POST   /login                                       token check (always 200 for authenticated callers)
//
// Every response carries "Content-Version: 1"; errors are RFC 7807
// problem documents. Scopes and names are case-insensitive. Remote
// repos proxy another SE-0292 registry (archives and release info are
// cached forever; release lists and identifier lookups honour
// MutableTTL). Virtual repos merge release lists across members.
//
// Client configuration:
//
//	swift package-registry set https://kutu.example.com/registries/{ns}/{repo}
//	swift package-registry login https://kutu.example.com/registries/{ns}/{repo} --token <kutu token>
//	swift package-registry publish scope.name 1.0.0
package swift

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeSwift

const (
	acceptJSON  = "application/vnd.swift.registry.v1+json"
	acceptZip   = "application/vnd.swift.registry.v1+zip"
	acceptSwift = "application/vnd.swift.registry.v1+swift"
)

var (
	scopeRe    = regexp.MustCompile(`^[a-zA-Z0-9](?:-?[a-zA-Z0-9]){0,38}$`)
	nameRe     = regexp.MustCompile(`^[a-zA-Z0-9](?:[-_]?[a-zA-Z0-9]){0,99}$`)
	versionRe  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+\-]{0,127}$`)
	manifestRe = regexp.MustCompile(`^Package@swift-(\d+)(?:\.(\d+))?(?:\.(\d+))?\.swift$`)
	toolsRe    = regexp.MustCompile(`^//\s*swift-tools-version:\s*([0-9][0-9A-Za-z.\-]*)`)
	acceptVRe  = regexp.MustCompile(`application/vnd\.swift\.registry\.v([^+;,\s]+)`)
)

// ── Wire types ──

type signing struct {
	SignatureBase64Encoded string `json:"signatureBase64Encoded"`
	SignatureFormat        string `json:"signatureFormat"`
}

type resource struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Checksum string   `json:"checksum"`
	Signing  *signing `json:"signing,omitempty"`
}

type releaseInfo struct {
	ID          string          `json:"id"`
	Version     string          `json:"version"`
	Resources   []resource      `json:"resources"`
	Metadata    json.RawMessage `json:"metadata"`
	PublishedAt string          `json:"publishedAt,omitempty"`
}

type releaseRef struct {
	URL     string          `json:"url,omitempty"`
	Problem json.RawMessage `json:"problem,omitempty"`
}

type releaseList struct {
	Releases map[string]releaseRef `json:"releases"`
}

// releaseMeta is the per-release sidecar stored by local repos.
type releaseMeta struct {
	ID              string            `json:"id"`
	Version         string            `json:"version"`
	Checksum        string            `json:"checksum"`
	Size            int64             `json:"size"`
	Metadata        json.RawMessage   `json:"metadata,omitempty"`
	PublishedAt     string            `json:"publishedAt"`
	Manifests       map[string]string `json:"manifests,omitempty"`
	Signature       string            `json:"signature,omitempty"`
	SignatureFormat string            `json:"signatureFormat,omitempty"`
}

type releaseMetadata struct {
	Description             string   `json:"description"`
	LicenseURL              string   `json:"licenseURL"`
	ReadmeURL               string   `json:"readmeURL"`
	RepositoryURLs          []string `json:"repositoryURLs"`
	OriginalPublicationTime string   `json:"originalPublicationTime"`
	Author                  *struct {
		Name string `json:"name"`
	} `json:"author"`
}

func parseMetadata(raw json.RawMessage) releaseMetadata {
	var m releaseMetadata
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

func (m *releaseMeta) info() releaseInfo {
	res := resource{Name: "source-archive", Type: "application/zip", Checksum: m.Checksum}
	if m.Signature != "" {
		res.Signing = &signing{SignatureBase64Encoded: m.Signature, SignatureFormat: m.SignatureFormat}
	}
	md := m.Metadata
	if len(md) == 0 {
		md = json.RawMessage("{}")
	}
	return releaseInfo{ID: m.ID, Version: m.Version, Resources: []resource{res}, Metadata: md, PublishedAt: m.PublishedAt}
}

// publishedTime prefers metadata.originalPublicationTime.
func publishedTime(info releaseInfo) time.Time {
	if t, err := time.Parse(time.RFC3339, parseMetadata(info.Metadata).OriginalPublicationTime); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, info.PublishedAt)
	return t
}

// ── HTTP helpers ──

func setVersion(w http.ResponseWriter) { w.Header().Set("Content-Version", "1") }

func writeJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	body, err := pkgbase.MarshalJSON(v)
	if err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return
	}
	setVersion(w)
	pkgbase.WriteBytes(w, r, code, "application/json", body)
}

func problem(w http.ResponseWriter, code int, detail string) {
	body, _ := pkgbase.MarshalJSON(map[string]any{"detail": detail})
	setVersion(w)
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Language", "en")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func notFound(w http.ResponseWriter, what string) { problem(w, http.StatusNotFound, what+" not found") }

// checkAccept rejects unknown API versions (400) and unsupported ones (415).
func checkAccept(w http.ResponseWriter, r *http.Request) bool {
	m := acceptVRe.FindStringSubmatch(r.Header.Get("Accept"))
	if m == nil || m[1] == "1" {
		return true
	}
	for _, c := range m[1] {
		if c < '0' || c > '9' {
			problem(w, http.StatusBadRequest, "invalid API version")
			return false
		}
	}
	problem(w, http.StatusUnsupportedMediaType, "unsupported API version")
	return false
}

func upstreamErr(w http.ResponseWriter, err error, what string) {
	if pkgbase.IsNotFound(err) {
		notFound(w, what)
		return
	}
	problem(w, http.StatusBadGateway, "upstream: "+err.Error())
}

func linkHeader(w http.ResponseWriter, target, rel string) {
	w.Header().Add("Link", fmt.Sprintf("<%s>; rel=%q", target, rel))
}

// ── Routing ──

type routeKind int

const (
	rNone routeKind = iota
	rIdentifiers
	rLogin
	rList
	rInfo
	rArchive
	rManifest
)

type route struct {
	kind              routeKind
	scope, name, ver  string
	scopeKey, nameKey string // lower-cased storage keys
}

func (rt route) id() string { return rt.scope + "." + rt.name }
func (rt route) key() string {
	return rt.scopeKey + "." + rt.nameKey
}

func parseRoute(p string) route {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		if u, err := url.PathUnescape(s); err == nil {
			parts[i] = u
		}
	}
	if len(parts) == 1 {
		switch parts[0] {
		case "identifiers":
			return route{kind: rIdentifiers}
		case "login":
			return route{kind: rLogin}
		}
		return route{}
	}
	if len(parts) < 2 || len(parts) > 4 {
		return route{}
	}
	rt := route{scope: parts[0], name: parts[1]}
	switch len(parts) {
	case 2:
		rt.kind = rList
		rt.name = strings.TrimSuffix(rt.name, ".json")
	case 3:
		if strings.HasSuffix(parts[2], ".zip") {
			rt.kind, rt.ver = rArchive, strings.TrimSuffix(parts[2], ".zip")
		} else {
			rt.kind, rt.ver = rInfo, strings.TrimSuffix(parts[2], ".json")
		}
	case 4:
		if parts[3] != "Package.swift" {
			return route{}
		}
		rt.kind, rt.ver = rManifest, parts[2]
	}
	if !scopeRe.MatchString(rt.scope) || !nameRe.MatchString(rt.name) || (rt.ver != "" && !versionRe.MatchString(rt.ver)) {
		return route{}
	}
	rt.scopeKey, rt.nameKey = strings.ToLower(rt.scope), strings.ToLower(rt.name)
	return rt
}

// splitID splits "scope.name" into lower-cased storage keys.
func splitID(id string) (string, string, bool) {
	scope, name, ok := strings.Cut(strings.TrimSpace(id), ".")
	if !ok || !scopeRe.MatchString(scope) || !nameRe.MatchString(name) {
		return "", "", false
	}
	return strings.ToLower(scope), strings.ToLower(name), true
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	rt := parseRoute(r.URL.Path)
	switch rt.kind {
	case rArchive:
		return registry.ArtifactRef{Name: rt.key(), Version: rt.ver}, true
	case rList, rInfo, rManifest:
		return registry.ArtifactRef{Name: rt.key()}, true
	}
	return registry.ArtifactRef{}, false
}

func releaseURL(base string, rt route, ver string) string {
	return base + "/" + url.PathEscape(rt.scope) + "/" + url.PathEscape(rt.name) + "/" + url.PathEscape(ver)
}

// latestOf returns the highest version among available releases.
func latestOf(l releaseList) string {
	var vs []string
	for v, ref := range l.Releases {
		if len(ref.Problem) == 0 {
			vs = append(vs, v)
		}
	}
	return pkgbase.Latest(vs)
}

func sortedVersions(l releaseList) []string {
	vs := make([]string, 0, len(l.Releases))
	for v := range l.Releases {
		vs = append(vs, v)
	}
	pkgbase.SortVersions(vs)
	return vs
}

// writeList rewrites release URLs to base and writes the listing.
func writeList(w http.ResponseWriter, r *http.Request, rt route, l releaseList, canonical string) {
	out := releaseList{Releases: make(map[string]releaseRef, len(l.Releases))}
	for v, ref := range l.Releases {
		out.Releases[v] = releaseRef{URL: releaseURL(pkgbase.PublicBase(r), rt, v), Problem: ref.Problem}
	}
	if canonical != "" {
		linkHeader(w, canonical, "canonical")
	}
	if latest := latestOf(out); latest != "" {
		linkHeader(w, releaseURL(pkgbase.PublicBase(r), rt, latest), "latest-version")
	}
	writeJSON(w, r, http.StatusOK, out)
}

// infoLinks adds latest/successor/predecessor Link headers.
func infoLinks(w http.ResponseWriter, r *http.Request, rt route, versions []string) {
	base := pkgbase.PublicBase(r)
	pkgbase.SortVersions(versions)
	if len(versions) > 0 {
		linkHeader(w, releaseURL(base, rt, versions[len(versions)-1]), "latest-version")
	}
	for i, v := range versions {
		if v != rt.ver {
			continue
		}
		if i+1 < len(versions) {
			linkHeader(w, releaseURL(base, rt, versions[i+1]), "successor-version")
		}
		if i > 0 {
			linkHeader(w, releaseURL(base, rt, versions[i-1]), "predecessor-version")
		}
	}
}

func serveManifest(w http.ResponseWriter, r *http.Request, rt route, manifests map[string]string) {
	base := releaseURL(pkgbase.PublicBase(r), rt, rt.ver) + "/Package.swift"
	file := "Package.swift"
	if sv := r.URL.Query().Get("swift-version"); sv != "" {
		file = "Package@swift-" + sv + ".swift"
		if _, ok := manifests[file]; !ok {
			setVersion(w)
			w.Header().Set("Location", base)
			w.WriteHeader(http.StatusSeeOther)
			return
		}
	}
	body, ok := manifests[file]
	if !ok {
		notFound(w, "manifest")
		return
	}
	if file == "Package.swift" {
		names := make([]string, 0, len(manifests))
		for n := range manifests {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			m := manifestRe.FindStringSubmatch(n)
			if m == nil {
				continue
			}
			sv := strings.TrimSuffix(strings.TrimPrefix(n, "Package@swift-"), ".swift")
			link := fmt.Sprintf("<%s?swift-version=%s>; rel=\"alternate\"; filename=%q", base, url.QueryEscape(sv), n)
			if tv := toolsVersion(manifests[n]); tv != "" {
				link += fmt.Sprintf("; swift-tools-version=%q", tv)
			}
			w.Header().Add("Link", link)
		}
	}
	setVersion(w)
	w.Header().Set("Cache-Control", "public, immutable")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file))
	pkgbase.WriteBytes(w, r, http.StatusOK, "text/x-swift", []byte(body))
}

func toolsVersion(manifest string) string {
	line, _, _ := strings.Cut(manifest, "\n")
	if m := toolsRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
		return m[1]
	}
	return ""
}

func serveArchive(w http.ResponseWriter, r *http.Request, rt route, body []byte, sig, sigFormat string) {
	sum, _ := hex.DecodeString(pkgbase.SHA256Hex(body))
	setVersion(w)
	w.Header().Set("Cache-Control", "public, immutable")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(sum))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", rt.name+"-"+rt.ver+".zip"))
	if sig != "" {
		w.Header().Set("X-Swift-Package-Signature-Format", sigFormat)
		w.Header().Set("X-Swift-Package-Signature", sig)
	}
	w.Header().Set("Content-Type", "application/zip")
	http.ServeContent(w, r, rt.name+"-"+rt.ver+".zip", time.Time{}, bytes.NewReader(body))
}

// normURL canonicalises a repository URL for identifier lookups.
func normURL(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	} else if at := strings.Index(u, "@"); at >= 0 {
		// scp-like git@host:path
		u = strings.Replace(u, ":", "/", 1)
	}
	if at := strings.Index(u, "@"); at >= 0 && at < strings.Index(u+"/", "/") {
		u = u[at+1:]
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return strings.TrimSuffix(u, "/")
}

// ── Archive parsing ──

// extractManifests returns Package.swift and Package@swift-*.swift from
// the shallowest directory of a source archive that has Package.swift.
func extractManifests(archive []byte) (map[string]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("source archive is not a valid zip: %w", err)
	}
	byDir := map[string]map[string]string{}
	for _, f := range zr.File {
		dir, base := path.Split(f.Name)
		dir = strings.Trim(dir, "/")
		if strings.Count(dir, "/") > 0 || f.FileInfo().IsDir() {
			continue
		}
		if base != "Package.swift" && !manifestRe.MatchString(base) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		byDir[dir][base] = string(b)
	}
	if m, ok := byDir[""]; ok && m["Package.swift"] != "" {
		return m, nil
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if _, ok := byDir[d]["Package.swift"]; ok {
			return byDir[d], nil
		}
	}
	return nil, errors.New("package doesn't contain a valid manifest (Package.swift) file")
}

// ── Store ──

// Store wraps pkgbase.Store with the swift layout:
//
//	archives/{scope}/{name}/{ver}.zip
//	releases/{scope}/{name}/{ver}.json     release sidecar (local)
//	upstream/{scope}/{name}/{ver}.json     cached upstream release info (remote)
//	upstream/{scope}/{name}/{ver}/{file}   cached upstream manifests (remote)
//	mutable/…                              TTL-bound upstream documents (remote)
type Store struct {
	*pkgbase.Store
	mu *sync.Mutex
}

func wrapStore(s *pkgbase.Store) *Store { return &Store{Store: s, mu: &sync.Mutex{}} }

func archiveRel(scope, name, ver string) string {
	return path.Join("archives", scope, name, ver+".zip")
}
func metaRel(scope, name, ver string) string {
	return path.Join("releases", scope, name, ver+".json")
}

// versionsIn lists "{ver}{suffix}" files under dir (ascending).
func (s *Store) versionsIn(dir, suffix string) []string {
	files, _ := s.ListFiles(dir)
	var out []string
	for _, f := range files {
		if v, ok := strings.CutSuffix(f.Name, suffix); ok && v != "" && !strings.HasPrefix(v, "_") {
			out = append(out, v)
		}
	}
	pkgbase.SortVersions(out)
	return out
}

// listUnder lists "scope.name" packages found under root.
func (s *Store) listUnder(root, suffix string) ([]registry.PackageSummary, error) {
	scopes, err := s.ListDirs(root)
	if err != nil {
		return nil, err
	}
	out := []registry.PackageSummary{}
	for _, sc := range scopes {
		names, _ := s.ListDirs(path.Join(root, sc))
		for _, n := range names {
			if vs := s.versionsIn(path.Join(root, sc, n), suffix); len(vs) > 0 {
				out = append(out, registry.PackageSummary{Name: sc + "." + n, Versions: vs})
			}
		}
	}
	return out, nil
}

func (s *Store) readMeta(scope, name, ver string) (*releaseMeta, error) {
	b, err := s.Read(metaRel(scope, name, ver))
	if err != nil {
		return nil, err
	}
	var m releaseMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) writeMeta(scope, name string, m *releaseMeta) error {
	b, err := pkgbase.MarshalJSON(m)
	if err != nil {
		return err
	}
	return s.Write(metaRel(scope, name, m.Version), b)
}

func detailFromInfos(id string, infos []releaseInfo, sizeOf func(ver string) int64) *registry.PackageDetail {
	sort.SliceStable(infos, func(i, j int) bool { return pkgbase.CompareVersions(infos[i].Version, infos[j].Version) > 0 })
	g := &registry.GenericPackageDetail{}
	if len(infos) > 0 {
		g.LatestVersion = infos[0].Version
		md := parseMetadata(infos[0].Metadata)
		g.Description = md.Description
		if len(md.RepositoryURLs) > 0 {
			g.Homepage = md.RepositoryURLs[0]
		}
		g.Metadata = map[string]string{}
		if md.LicenseURL != "" {
			g.Metadata["license_url"] = md.LicenseURL
		}
		if md.ReadmeURL != "" {
			g.Metadata["readme_url"] = md.ReadmeURL
		}
		if md.Author != nil && md.Author.Name != "" {
			g.Metadata["author"] = md.Author.Name
		}
		if len(md.RepositoryURLs) > 0 {
			g.Metadata["repository_urls"] = strings.Join(md.RepositoryURLs, ", ")
		}
		if len(g.Metadata) == 0 {
			g.Metadata = nil
		}
	}
	for _, info := range infos {
		row := registry.GenericVersionDetail{Version: info.Version}
		if t := publishedTime(info); !t.IsZero() {
			row.PublishedAt = t.UTC().Format(time.RFC3339)
		}
		if sizeOf != nil {
			row.Size = sizeOf(info.Version)
		}
		for _, res := range info.Resources {
			if res.Name == "source-archive" {
				_, n, _ := strings.Cut(id, ".")
				row.Files = []registry.GenericFile{{Name: n + "-" + info.Version + ".zip", Size: row.Size, SHA256: res.Checksum}}
				if res.Signing != nil {
					row.Metadata = map[string]string{"signature_format": res.Signing.SignatureFormat}
				}
			}
		}
		g.Versions = append(g.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: id, Generic: g}
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

func (l *Local) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return l.store.listUnder("releases", ".json")
}

func (l *Local) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, l, l.store.Store), nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	scope, n, ok := splitID(name)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	vs := l.store.versionsIn(path.Join("releases", scope, n), ".json")
	var infos []releaseInfo
	id := scope + "." + n
	for _, v := range vs {
		if m, err := l.store.readMeta(scope, n, v); err == nil {
			infos = append(infos, m.info())
			id = m.ID
		}
	}
	if len(infos) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	return detailFromInfos(id, infos, func(v string) int64 {
		if m, err := l.store.readMeta(scope, n, v); err == nil {
			return m.Size
		}
		return 0
	}), nil
}

func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	scope, n, ok := splitID(name)
	if !ok || !versionRe.MatchString(version) || !l.store.Exists(metaRel(scope, n, version)) {
		return registry.ErrPackageNotFound
	}
	if err := l.store.Delete(archiveRel(scope, n, version)); err != nil {
		return err
	}
	if err := l.store.Delete(metaRel(scope, n, version)); err != nil {
		return err
	}
	l.EmitDeleted(scope + "." + n + "@" + version)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	scope, n, ok := splitID(ref.Name)
	if !ok || !versionRe.MatchString(ref.Version) {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	m, err := l.store.readMeta(scope, n, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	return registry.ArtifactMeta{PublishedAt: publishedTime(m.info())}, nil
}

func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("swift: promote target %T is not a swift local registry", dst)
	}
	scope, n, ok := splitID(name)
	if !ok || !versionRe.MatchString(version) {
		return registry.ErrPackageNotFound
	}
	m, err := l.store.readMeta(scope, n, version)
	if err != nil {
		return registry.ErrPackageNotFound
	}
	body, err := l.store.Read(archiveRel(scope, n, version))
	if err != nil {
		return err
	}
	if err := d.store.Write(archiveRel(scope, n, version), body); err != nil {
		return err
	}
	if err := d.store.writeMeta(scope, n, m); err != nil {
		return err
	}
	d.EmitPublished(scope+"."+n+"@"+version, int64(len(body)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !checkAccept(w, r) {
		return
	}
	rt := parseRoute(r.URL.Path)
	read := pkgbase.IsRead(r)
	switch {
	case rt.kind == rLogin && r.Method == http.MethodPost:
		setVersion(w)
		w.WriteHeader(http.StatusOK)
	case rt.kind == rIdentifiers && read:
		l.identifiers(w, r)
	case rt.kind == rList && read:
		l.list(w, r, rt)
	case rt.kind == rInfo && read:
		m, err := l.store.readMeta(rt.scopeKey, rt.nameKey, rt.ver)
		if err != nil {
			notFound(w, "release")
			return
		}
		infoLinks(w, r, rt, l.store.versionsIn(path.Join("releases", rt.scopeKey, rt.nameKey), ".json"))
		writeJSON(w, r, http.StatusOK, m.info())
	case rt.kind == rManifest && read:
		m, err := l.store.readMeta(rt.scopeKey, rt.nameKey, rt.ver)
		if err != nil {
			notFound(w, "release")
			return
		}
		serveManifest(w, r, rt, m.Manifests)
	case rt.kind == rArchive && read:
		m, err := l.store.readMeta(rt.scopeKey, rt.nameKey, rt.ver)
		if err != nil {
			notFound(w, "release")
			return
		}
		body, err := l.store.Read(archiveRel(rt.scopeKey, rt.nameKey, rt.ver))
		if err != nil {
			notFound(w, "source archive")
			return
		}
		serveArchive(w, r, rt, body, m.Signature, m.SignatureFormat)
	case rt.kind == rInfo && r.Method == http.MethodPut:
		l.publish(w, r, rt)
	case rt.kind == rInfo && r.Method == http.MethodDelete:
		if !l.checkPush(w) {
			return
		}
		if err := l.DeleteVersion(r.Context(), rt.key(), rt.ver); err != nil {
			if pkgbase.IsNotFound(err) {
				notFound(w, "release")
				return
			}
			problem(w, http.StatusInternalServerError, err.Error())
			return
		}
		setVersion(w)
		w.WriteHeader(http.StatusNoContent)
	case rt.kind == rNone:
		notFound(w, "resource")
	default:
		problem(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) checkPush(w http.ResponseWriter) bool {
	if !l.AllowPush {
		problem(w, http.StatusMethodNotAllowed, "publishing isn't supported")
		return false
	}
	return true
}

func (l *Local) list(w http.ResponseWriter, r *http.Request, rt route) {
	vs := l.store.versionsIn(path.Join("releases", rt.scopeKey, rt.nameKey), ".json")
	if len(vs) == 0 {
		notFound(w, "package")
		return
	}
	lst := releaseList{Releases: map[string]releaseRef{}}
	for _, v := range vs {
		lst.Releases[v] = releaseRef{}
	}
	var canonical string
	if m, err := l.store.readMeta(rt.scopeKey, rt.nameKey, latestOf(lst)); err == nil {
		if urls := parseMetadata(m.Metadata).RepositoryURLs; len(urls) > 0 {
			canonical = urls[0]
		}
	}
	writeList(w, r, rt, lst, canonical)
}

func (l *Local) identifiers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("url")
	if q == "" {
		problem(w, http.StatusBadRequest, "missing url parameter")
		return
	}
	want := normURL(q)
	pkgs, _ := l.ListPackages(r.Context())
	ids := []string{}
	for _, p := range pkgs {
		scope, n, _ := splitID(p.Name)
		for i := len(p.Versions) - 1; i >= 0; i-- {
			m, err := l.store.readMeta(scope, n, p.Versions[i])
			if err != nil {
				continue
			}
			matched := false
			for _, u := range parseMetadata(m.Metadata).RepositoryURLs {
				if normURL(u) == want {
					matched = true
					break
				}
			}
			if matched {
				ids = append(ids, m.ID)
				break
			}
		}
	}
	if len(ids) == 0 {
		notFound(w, "identifiers")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"identifiers": ids})
}

func readPart(p io.Reader, cte string, max int64) ([]byte, error) {
	b, err := pkgbase.ReadBody(p, max)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(cte), "base64") {
		return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b)), ""))
	}
	return b, nil
}

func (l *Local) publish(w http.ResponseWriter, r *http.Request, rt route) {
	if !l.checkPush(w) {
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		problem(w, http.StatusUnsupportedMediaType, "expected multipart/form-data")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	var archive, metadata, sig []byte
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			problem(w, http.StatusBadRequest, err.Error())
			return
		}
		cte := part.Header.Get("Content-Transfer-Encoding")
		var perr error
		switch part.FormName() {
		case "source-archive":
			archive, perr = readPart(part, cte, l.MaxUpload)
		case "metadata":
			metadata, perr = readPart(part, cte, 1<<20)
		case "source-archive-signature":
			sig, perr = readPart(part, cte, 1<<20)
		}
		_ = part.Close()
		if perr != nil {
			problem(w, http.StatusRequestEntityTooLarge, perr.Error())
			return
		}
	}
	if len(archive) == 0 {
		problem(w, http.StatusUnprocessableEntity, "missing source-archive part")
		return
	}
	metadata = bytes.TrimSpace(metadata)
	if len(metadata) > 0 {
		var obj map[string]any
		if json.Unmarshal(metadata, &obj) != nil {
			problem(w, http.StatusUnprocessableEntity, "invalid JSON provided for release metadata")
			return
		}
	}
	manifests, err := extractManifests(archive)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if l.store.Exists(metaRel(rt.scopeKey, rt.nameKey, rt.ver)) {
		problem(w, http.StatusConflict, "a release with version "+rt.ver+" already exists")
		return
	}
	if code, err := l.Guard.Check(l.store.Store, false, int64(len(archive))); err != nil {
		problem(w, code, err.Error())
		return
	}
	if err := l.store.Write(archiveRel(rt.scopeKey, rt.nameKey, rt.ver), archive); err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return
	}
	m := &releaseMeta{
		ID: rt.id(), Version: rt.ver, Checksum: pkgbase.SHA256Hex(archive), Size: int64(len(archive)),
		Metadata: json.RawMessage(metadata), PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Manifests: manifests,
	}
	if len(sig) > 0 {
		m.Signature = base64.StdEncoding.EncodeToString(sig)
		m.SignatureFormat = r.Header.Get("X-Swift-Package-Signature-Format")
		if m.SignatureFormat == "" {
			m.SignatureFormat = "cms-1.0.0"
		}
	}
	if err := l.store.writeMeta(rt.scopeKey, rt.nameKey, m); err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(rt.key()+"@"+rt.ver, int64(len(archive)))
	setVersion(w)
	w.Header().Set("Location", releaseURL(pkgbase.PublicBase(r), rt, rt.ver))
	w.WriteHeader(http.StatusCreated)
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
		return &Remote{RemoteRepo: base, store: wrapStore(base.Store)}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

// fetch is FetchCached with an Accept header (SE-0292 servers may
// reject requests without one).
func (rr *Remote) fetch(ctx context.Context, cacheRel, upath, accept string, mutable bool) ([]byte, error) {
	if rr.store.Exists(cacheRel) && (!mutable || rr.Fresh(cacheRel)) {
		if b, err := rr.store.Read(cacheRel); err == nil {
			return b, nil
		}
	}
	client := rr.Client
	if rr.Router != nil {
		client = rr.Router.For(upath)
	}
	resp, err := client.GetWithHeaders(ctx, upath, http.Header{"Accept": {accept}})
	if err != nil {
		if b, rerr := rr.store.Read(cacheRel); rerr == nil && !errors.Is(err, upstream.ErrNotFound) {
			return b, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = rr.store.Write(cacheRel, body)
	return body, nil
}

func upPath(rt route, ver, suffix string) string {
	p := "/" + url.PathEscape(rt.scope) + "/" + url.PathEscape(rt.name)
	if ver != "" {
		p += "/" + url.PathEscape(ver)
	}
	return p + suffix
}

func (rr *Remote) releases(ctx context.Context, rt route) (releaseList, error) {
	var l releaseList
	body, err := rr.fetch(ctx, path.Join("mutable", rt.scopeKey, rt.nameKey+".json"), upPath(rt, "", ""), acceptJSON, true)
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(body, &l); err != nil {
		return l, fmt.Errorf("swift: invalid upstream release list: %w", err)
	}
	return l, nil
}

func (rr *Remote) info(ctx context.Context, rt route, ver string) (releaseInfo, error) {
	var info releaseInfo
	rel := path.Join("upstream", rt.scopeKey, rt.nameKey, ver+".json")
	body, err := rr.fetch(ctx, rel, upPath(rt, ver, ""), acceptJSON, false)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(body, &info); err != nil {
		_ = rr.store.Delete(rel)
		return info, fmt.Errorf("swift: invalid upstream release info: %w", err)
	}
	return info, nil
}

func (rr *Remote) archive(ctx context.Context, rt route) ([]byte, releaseInfo, error) {
	info, ierr := rr.info(ctx, rt, rt.ver)
	rel := archiveRel(rt.scopeKey, rt.nameKey, rt.ver)
	body, err := rr.fetch(ctx, rel, upPath(rt, rt.ver, ".zip"), acceptZip, false)
	if err != nil {
		return nil, info, err
	}
	if ierr == nil {
		for _, res := range info.Resources {
			if res.Name == "source-archive" && res.Checksum != "" && !strings.EqualFold(res.Checksum, pkgbase.SHA256Hex(body)) {
				_ = rr.store.Delete(rel)
				return nil, info, fmt.Errorf("swift: source archive checksum mismatch for %s %s", rt.id(), rt.ver)
			}
		}
	}
	return body, info, nil
}

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return rr.store.listUnder("archives", ".zip")
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	scope, n, ok := splitID(name)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	dir := path.Join("upstream", scope, n)
	var infos []releaseInfo
	id := scope + "." + n
	for _, v := range rr.store.versionsIn(dir, ".json") {
		b, err := rr.store.Read(path.Join(dir, v+".json"))
		if err != nil {
			continue
		}
		var info releaseInfo
		if json.Unmarshal(b, &info) == nil {
			infos = append(infos, info)
			if info.ID != "" {
				id = info.ID
			}
		}
	}
	if len(infos) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	return detailFromInfos(id, infos, func(v string) int64 {
		if fi, err := rr.store.Stat(archiveRel(scope, n, v)); err == nil {
			return fi.Size
		}
		return 0
	}), nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, "mutable"), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func routeFor(name, version string) (route, bool) {
	scope, n, ok := splitID(name)
	if !ok || (version != "" && !versionRe.MatchString(version)) {
		return route{}, false
	}
	return route{scope: scope, name: n, scopeKey: scope, nameKey: n, ver: version}, true
}

func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	rt, ok := routeFor(ref.Name, ref.Version)
	if !ok || ref.Version == "" {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	info, err := rr.info(ctx, rt, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	return registry.ArtifactMeta{PublishedAt: publishedTime(info)}, nil
}

func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	rt, ok := routeFor(name, version)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	if version == "" {
		l, err := rr.releases(ctx, rt)
		if err != nil {
			return err
		}
		if rt.ver = latestOf(l); rt.ver == "" {
			return registry.ErrPackageNotFound
		}
	}
	if _, err := rr.fetch(ctx, path.Join("upstream", rt.scopeKey, rt.nameKey, rt.ver, "Package.swift"), upPath(rt, rt.ver, "/Package.swift"), acceptSwift, false); err != nil {
		return err
	}
	_, _, err := rr.archive(ctx, rt)
	return err
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !checkAccept(w, r) {
		return
	}
	rt := parseRoute(r.URL.Path)
	if rt.kind == rLogin && r.Method == http.MethodPost {
		setVersion(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !pkgbase.IsRead(r) {
		problem(w, http.StatusMethodNotAllowed, "publishing isn't supported")
		return
	}
	ctx := r.Context()
	switch rt.kind {
	case rList:
		l, err := rr.releases(ctx, rt)
		if err != nil {
			upstreamErr(w, err, "package")
			return
		}
		writeList(w, r, rt, l, "")
	case rInfo:
		info, err := rr.info(ctx, rt, rt.ver)
		if err != nil {
			upstreamErr(w, err, "release")
			return
		}
		if l, err := rr.releases(ctx, rt); err == nil {
			infoLinks(w, r, rt, sortedVersions(l))
		}
		writeJSON(w, r, http.StatusOK, info)
	case rManifest:
		file := "Package.swift"
		upath := upPath(rt, rt.ver, "/Package.swift")
		if sv := r.URL.Query().Get("swift-version"); sv != "" {
			file = "Package@swift-" + sv + ".swift"
			upath += "?swift-version=" + url.QueryEscape(sv)
		}
		body, err := rr.fetch(ctx, path.Join("upstream", rt.scopeKey, rt.nameKey, rt.ver, file), upath, acceptSwift, false)
		if err != nil {
			upstreamErr(w, err, "manifest")
			return
		}
		setVersion(w)
		w.Header().Set("Cache-Control", "public, immutable")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file))
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/x-swift", body)
	case rArchive:
		body, info, err := rr.archive(ctx, rt)
		if err != nil {
			upstreamErr(w, err, "source archive")
			return
		}
		var sig, format string
		for _, res := range info.Resources {
			if res.Name == "source-archive" && res.Signing != nil {
				sig, format = res.Signing.SignatureBase64Encoded, res.Signing.SignatureFormat
			}
		}
		serveArchive(w, r, rt, body, sig, format)
	case rIdentifiers:
		q := r.URL.Query().Get("url")
		if q == "" {
			problem(w, http.StatusBadRequest, "missing url parameter")
			return
		}
		body, err := rr.fetch(ctx, path.Join("mutable", "_identifiers", pkgbase.SHA256Hex([]byte(q))+".json"), "/identifiers?url="+url.QueryEscape(q), acceptJSON, true)
		if err != nil {
			upstreamErr(w, err, "identifiers")
			return
		}
		var res struct {
			Identifiers []string `json:"identifiers"`
		}
		if json.Unmarshal(body, &res) != nil || len(res.Identifiers) == 0 {
			notFound(w, "identifiers")
			return
		}
		writeJSON(w, r, http.StatusOK, res)
	default:
		notFound(w, "resource")
	}
}

// ── Virtual ──

// Virtual merges release lists and identifier lookups across members.
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
	if !checkAccept(w, r) {
		return
	}
	rt := parseRoute(r.URL.Path)
	if rt.kind == rLogin && r.Method == http.MethodPost {
		setVersion(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !pkgbase.IsRead(r) {
		problem(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	switch rt.kind {
	case rList:
		merged := releaseList{Releases: map[string]releaseRef{}}
		for _, body := range v.CollectMembers(r) {
			var l releaseList
			if json.Unmarshal(body, &l) != nil {
				continue
			}
			for ver, ref := range l.Releases {
				if prev, ok := merged.Releases[ver]; !ok || (len(prev.Problem) > 0 && len(ref.Problem) == 0) {
					merged.Releases[ver] = ref
				}
			}
		}
		if len(merged.Releases) == 0 {
			notFound(w, "package")
			return
		}
		writeList(w, r, rt, merged, "")
	case rIdentifiers:
		seen := map[string]bool{}
		ids := []string{}
		for _, body := range v.CollectMembers(r) {
			var res struct {
				Identifiers []string `json:"identifiers"`
			}
			if json.Unmarshal(body, &res) != nil {
				continue
			}
			for _, id := range res.Identifiers {
				if k := strings.ToLower(id); !seen[k] {
					seen[k] = true
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			notFound(w, "identifiers")
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"identifiers": ids})
	default:
		if !v.ServeFirstHit(w, r) {
			notFound(w, "resource")
		}
	}
}
