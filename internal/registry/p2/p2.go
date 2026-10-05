// Package p2 implements an Eclipse p2 update site registry.
//
// Local repos host any number of p2 sites side by side and expose the
// repo root as a composite of all of them:
//
//	GET    /p2.index                       root p2.index (composite)
//	GET    /compositeContent.xml           composite metadata over every site
//	GET    /compositeArtifacts.xml         composite artifacts over every site
//	GET    /                               JSON site list
//	GET    /{site}/{path}                  stored site file (content.jar, plugins/…, …)
//	PUT    /upload/{site}                  upload a zipped p2 repository (replaces the site)
//	PUT    /{site}/{path}                  upload a single file
//	DELETE /{site}                         delete a site
//	DELETE /{site}/{path}                  delete a single file
//
// The composite documents are generated per request from the stored
// sites, so they always reflect the current set. A site's "version"
// is its last upload timestamp (UTC, 20060102T150405Z).
//
// Remote repos proxy an update site: metadata (content.*, artifacts.*,
// composite*, p2.index, directory listings) honours MutableTTL, files
// under plugins/, features/ and binary/ are cached forever. Absolute
// composite children on the upstream host are rewritten to point back
// at kutu (plain .xml composites only; .jar metadata is passed through).
// Remote "packages" are the bundles (plugins/features) cached so far.
//
// Virtual repos publish a composite whose children are "{member}/";
// requests under /{member}/… are forwarded to that member, so no
// cross-repo URLs or extra credentials are needed.
//
// Client configuration: Eclipse → Help → Install New Software → Add…,
// location https://kutu.example.com/registries/{ns}/{repo}/ (Eclipse
// prompts for basic auth: any user, token as password), or in a
// Tycho/Maven build:
//
//	<repository><id>kutu</id><layout>p2</layout><url>https://kutu.example.com/registries/{ns}/{repo}/</url></repository>
package p2

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeP2

const (
	sitesDir   = "sites"
	siteMeta   = "meta/sites"
	filesDir   = "files"
	metaDir    = "meta"
	uploadPath = "upload"
	timeFormat = "20060102T150405Z"
)

const (
	metaRepoType     = "org.eclipse.equinox.internal.p2.metadata.repository.CompositeMetadataRepository"
	artifactRepoType = "org.eclipse.equinox.internal.p2.artifact.repository.CompositeArtifactRepository"
)

// rootIndex is the p2.index of every generated composite root.
const rootIndex = "version=1\nmetadata.repository.factory.order=compositeContent.xml,\\!\nartifact.repository.factory.order=compositeArtifacts.xml,\\!\n"

func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if r == '/' || r == '\\' || r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// validSite rejects names that would shadow the root documents.
func validSite(s string) bool {
	return validSegment(s) && s != uploadPath && !isRootDoc(s) && !strings.HasPrefix(s, ".")
}

func isRootDoc(s string) bool {
	return s == "p2.index" || strings.HasPrefix(s, "compositeContent.") || strings.HasPrefix(s, "compositeArtifacts.")
}

// cleanRel validates and normalises a site-relative file path.
func cleanRel(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", false
	}
	for _, s := range strings.Split(p, "/") {
		if !validSegment(s) {
			return "", false
		}
	}
	return p, true
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".xml"):
		return "application/xml"
	case strings.HasSuffix(name, ".jar"):
		return "application/java-archive"
	case strings.HasSuffix(name, ".xz"):
		return "application/x-xz"
	case strings.HasSuffix(name, ".index"), strings.HasSuffix(name, ".properties"):
		return "text/plain; charset=utf-8"
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	}
	return "application/octet-stream"
}

// isArtifactPath reports whether p addresses an immutable artifact.
func isArtifactPath(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 2 {
		return false
	}
	switch parts[len(parts)-2] {
	case "plugins", "features", "binary":
		return !strings.HasSuffix(p, "/")
	}
	return false
}

// bundleOf splits "plugins/{id}_{version}.jar" into id and version.
func bundleOf(p string) (id, version string, ok bool) {
	if !isArtifactPath(p) {
		return "", "", false
	}
	base := path.Base(p)
	for _, ext := range []string{".jar.pack.gz", ".jar", ".zip"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}
	for i := 0; i < len(base)-1; i++ {
		if base[i] == '_' && base[i+1] >= '0' && base[i+1] <= '9' && i > 0 {
			return base[:i], base[i+1:], true
		}
	}
	return "", "", false
}

// compositeXML renders a compositeContent / compositeArtifacts document.
func compositeXML(artifacts bool, name string, ts int64, children []string) []byte {
	pi, rt := "compositeMetadataRepository", metaRepoType
	if artifacts {
		pi, rt = "compositeArtifactRepository", artifactRepoType
	}
	var b bytes.Buffer
	b.WriteString("<?xml version='1.0' encoding='UTF-8'?>\n")
	fmt.Fprintf(&b, "<?%s version='1.0.0'?>\n", pi)
	fmt.Fprintf(&b, "<repository name='%s' type='%s' version='1.0.0'>\n", xmlAttr(name), rt)
	b.WriteString("  <properties size='2'>\n")
	fmt.Fprintf(&b, "    <property name='p2.timestamp' value='%d'/>\n", ts)
	b.WriteString("    <property name='p2.atomic.composite.loading' value='false'/>\n")
	b.WriteString("  </properties>\n")
	fmt.Fprintf(&b, "  <children size='%d'>\n", len(children))
	for _, c := range children {
		fmt.Fprintf(&b, "    <child location='%s'/>\n", xmlAttr(c))
	}
	b.WriteString("  </children>\n</repository>\n")
	return b.Bytes()
}

func xmlAttr(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", `"`, "&quot;")
	return r.Replace(s)
}

// serveRootDoc writes one of the generated composite root documents.
// ok=false when name is not a root document.
func serveRootDoc(w http.ResponseWriter, r *http.Request, name, repoName string, ts int64, children []string) bool {
	switch name {
	case "p2.index":
		pkgbase.WriteBytes(w, r, http.StatusOK, contentType(name), []byte(rootIndex))
	case "compositeContent.xml":
		pkgbase.WriteBytes(w, r, http.StatusOK, contentType(name), compositeXML(false, repoName, ts, children))
	case "compositeArtifacts.xml":
		pkgbase.WriteBytes(w, r, http.StatusOK, contentType(name), compositeXML(true, repoName, ts, children))
	default:
		return false
	}
	return true
}

// ── Store ──

// Store wraps pkgbase.Store with the p2 layout: sites/{site}/… for
// hosted sites (+ meta/sites/{site}.json) and files/… + meta/… for
// remote caches.
type Store struct{ *pkgbase.Store }

type siteInfo struct {
	Version  string `json:"version"`
	Uploaded string `json:"uploaded"`
}

func (s *Store) siteRel(site string, parts ...string) string {
	return path.Join(append([]string{sitesDir, site}, parts...)...)
}

func (s *Store) siteMetaRel(site string) string { return path.Join(siteMeta, site+".json") }

// Sites returns the hosted site names (sorted).
func (s *Store) Sites() []string {
	dirs, _ := s.ListDirs(sitesDir)
	var out []string
	for _, d := range dirs {
		if validSite(d) && s.siteExists(d) {
			out = append(out, d)
		}
	}
	return out
}

func (s *Store) info(site string) (siteInfo, bool) {
	var si siteInfo
	b, err := s.Read(s.siteMetaRel(site))
	if err != nil {
		return si, false
	}
	return si, json.Unmarshal(b, &si) == nil
}

func (s *Store) touch(site string, now time.Time) error {
	si := siteInfo{Version: now.UTC().Format(timeFormat), Uploaded: now.UTC().Format(time.RFC3339)}
	b, err := pkgbase.MarshalJSON(si)
	if err != nil {
		return err
	}
	return s.Write(s.siteMetaRel(site), b)
}

// siteExists reports whether site holds at least one file (empty
// directories may linger on backends without directory removal).
func (s *Store) siteExists(site string) bool {
	found := errors.New("found")
	return s.Walk(s.siteRel(site), func(string, rawfs.DirEntry) error { return found }) == found
}

func (s *Store) siteVersion(site string) string {
	if si, ok := s.info(site); ok {
		return si.Version
	}
	return ""
}

// ListPackages returns sites (local) and cached bundles (remote).
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	var out []registry.PackageSummary
	for _, site := range s.Sites() {
		ps := registry.PackageSummary{Name: site}
		if v := s.siteVersion(site); v != "" {
			ps.Versions = []string{v}
		}
		out = append(out, ps)
	}
	bundles := s.bundles()
	ids := make([]string, 0, len(bundles))
	for id := range bundles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, registry.PackageSummary{Name: id, Versions: bundles[id]})
	}
	return out, nil
}

// bundles maps cached remote bundle ids to versions (ascending).
func (s *Store) bundles() map[string][]string {
	set := map[string]map[string]struct{}{}
	_ = s.Walk(filesDir, func(rel string, _ rawfs.DirEntry) error {
		if id, v, ok := bundleOf(strings.TrimPrefix(rel, filesDir+"/")); ok {
			if set[id] == nil {
				set[id] = map[string]struct{}{}
			}
			set[id][v] = struct{}{}
		}
		return nil
	})
	out := map[string][]string{}
	for id, vs := range set {
		list := make([]string, 0, len(vs))
		for v := range vs {
			list = append(list, v)
		}
		pkgbase.SortVersions(list)
		out[id] = list
	}
	return out
}

// DeleteSite removes a site; version must be "", "*" or the recorded
// site version.
func (s *Store) DeleteSite(site, version string) error {
	if !validSite(site) || !s.siteExists(site) {
		return registry.ErrPackageNotFound
	}
	if version != "" && version != "*" && version != s.siteVersion(site) {
		return registry.ErrPackageNotFound
	}
	if _, _, errs := s.DeleteTree(s.siteRel(site)); len(errs) > 0 {
		return errs[0]
	}
	return s.Delete(s.siteMetaRel(site))
}

func (s *Store) latestTimestamp() int64 {
	var best time.Time
	for _, site := range s.Sites() {
		if si, ok := s.info(site); ok {
			if t, err := time.Parse(time.RFC3339, si.Uploaded); err == nil && t.After(best) {
				best = t
			}
		}
	}
	if best.IsZero() {
		return 0
	}
	return best.UnixMilli()
}

func siteDetail(s *Store, site string) (*registry.PackageDetail, error) {
	if !validSite(site) || !s.siteExists(site) {
		return nil, registry.ErrPackageNotFound
	}
	si, _ := s.info(site)
	row := registry.GenericVersionDetail{Version: si.Version, PublishedAt: si.Uploaded, Metadata: map[string]string{}}
	var plugins, features int
	_ = s.Walk(s.siteRel(site), func(rel string, e rawfs.DirEntry) error {
		inner := strings.TrimPrefix(rel, s.siteRel(site)+"/")
		row.Size += e.Size
		switch {
		case strings.HasPrefix(inner, "plugins/"):
			plugins++
		case strings.HasPrefix(inner, "features/"):
			features++
		default:
			row.Files = append(row.Files, registry.GenericFile{Name: inner, Size: e.Size})
		}
		return nil
	})
	row.Metadata["plugins"] = strconv.Itoa(plugins)
	row.Metadata["features"] = strconv.Itoa(features)
	d := &registry.GenericPackageDetail{LatestVersion: si.Version, Versions: []registry.GenericVersionDetail{row}}
	return &registry.PackageDetail{Type: typ, Name: site, Generic: d}, nil
}

func bundleDetail(s *Store, id string) (*registry.PackageDetail, error) {
	vs := s.bundles()[id]
	if len(vs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d := &registry.GenericPackageDetail{LatestVersion: pkgbase.Latest(vs)}
	for i := len(vs) - 1; i >= 0; i-- {
		d.Versions = append(d.Versions, registry.GenericVersionDetail{Version: vs[i]})
	}
	return &registry.PackageDetail{Type: typ, Name: id, Generic: d}, nil
}

// ── zip extraction ──

func hasMetadata(names map[string]bool, prefix string) bool {
	for _, n := range []string{"content.jar", "content.xml", "content.xml.xz", "compositeContent.jar", "compositeContent.xml"} {
		if names[prefix+n] {
			return true
		}
	}
	return false
}

type zipFile struct {
	rel string
	f   *zip.File
}

// siteFiles lists the files of a zipped p2 repository, stripping a
// single wrapping directory when the metadata lives one level down.
func siteFiles(zr *zip.Reader, maxTotal int64) ([]zipFile, error) {
	names := map[string]bool{}
	var files []*zip.File
	var total uint64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(f.Name, "__MACOSX/") {
			continue
		}
		rel, ok := cleanRel(strings.ReplaceAll(f.Name, "\\", "/"))
		if !ok {
			return nil, fmt.Errorf("unsafe path %q in archive", f.Name)
		}
		total += f.UncompressedSize64
		if maxTotal > 0 && total > uint64(maxTotal) {
			return nil, fmt.Errorf("archive expands beyond %d bytes", maxTotal)
		}
		names[rel] = true
		files = append(files, f)
	}
	prefix := ""
	if !hasMetadata(names, "") {
		tops := map[string]bool{}
		for n := range names {
			tops[strings.SplitN(n, "/", 2)[0]] = true
		}
		if len(tops) == 1 {
			for t := range tops {
				prefix = t + "/"
			}
		}
		if prefix == "" || !hasMetadata(names, prefix) {
			return nil, errors.New("archive is not a p2 repository (no content.jar/content.xml/compositeContent.* at its root)")
		}
	}
	out := make([]zipFile, 0, len(files))
	for _, f := range files {
		rel, _ := cleanRel(strings.ReplaceAll(f.Name, "\\", "/"))
		if !strings.HasPrefix(rel, prefix) {
			continue
		}
		out = append(out, zipFile{rel: strings.TrimPrefix(rel, prefix), f: f})
	}
	return out, nil
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
	return siteDetail(l.store, strings.Trim(name, "/"))
}

// DeleteVersion deletes the site name; version is "", "*" or the
// site's recorded upload timestamp.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	if err := l.store.DeleteSite(name, version); err != nil {
		return err
	}
	l.EmitDeleted(name)
	return nil
}

// ClassifyRequest maps /{site}/… to the site; artifact downloads
// (plugins/features/binary) carry the site version.
func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	parts := strings.SplitN(strings.Trim(r.URL.Path, "/"), "/", 2)
	if len(parts) == 0 || !validSite(parts[0]) {
		return registry.ArtifactRef{}, false
	}
	ref := registry.ArtifactRef{Name: parts[0]}
	if len(parts) == 2 && isArtifactPath(parts[1]) {
		ref.Version = l.store.siteVersion(parts[0])
	}
	return ref, true
}

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	si, ok := l.store.info(ref.Name)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	t, _ := time.Parse(time.RFC3339, si.Uploaded)
	return registry.ArtifactMeta{PublishedAt: t}, nil
}

// PromoteVersion copies the whole site into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("p2: promote target %s/%s is not a p2 local repository", dst.Namespace(), dst.Name())
	}
	if !validSite(name) || !l.store.siteExists(name) {
		return registry.ErrPackageNotFound
	}
	if version != "" && version != "*" && version != l.store.siteVersion(name) {
		return registry.ErrPackageNotFound
	}
	_, size := l.store.Usage(l.store.siteRel(name))
	if _, err := d.Guard.Check(d.store.Store, d.store.siteExists(name), size); err != nil {
		return err
	}
	if _, _, errs := d.store.DeleteTree(d.store.siteRel(name)); len(errs) > 0 {
		return errs[0]
	}
	err := l.store.Walk(l.store.siteRel(name), func(rel string, e rawfs.DirEntry) error {
		rc, _, err := l.store.Open(rel)
		if err != nil {
			return err
		}
		defer rc.Close()
		return d.store.WriteStream(rel, rc, e.Size)
	})
	if err != nil {
		return err
	}
	if b, err := l.store.Read(l.store.siteMetaRel(name)); err == nil {
		if err := d.store.Write(d.store.siteMetaRel(name), b); err != nil {
			return err
		}
	} else if err := d.store.touch(name, time.Now()); err != nil {
		return err
	}
	d.EmitPublished(name, size)
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(r.URL.Path, "/")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		l.serveRead(w, r, trimmed)
	case http.MethodPut, http.MethodPost:
		if !l.CheckPush(w) {
			return
		}
		if site, ok := strings.CutPrefix(trimmed, uploadPath+"/"); ok {
			l.uploadZip(w, r, site)
			return
		}
		l.uploadFile(w, r, trimmed)
	case http.MethodDelete:
		if !l.CheckPush(w) {
			return
		}
		l.remove(w, r, trimmed)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) serveRead(w http.ResponseWriter, r *http.Request, trimmed string) {
	if trimmed == "" {
		sites := l.store.Sites()
		out := make([]map[string]string, 0, len(sites))
		for _, s := range sites {
			out = append(out, map[string]string{"name": s, "version": l.store.siteVersion(s), "location": s + "/"})
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"sites": out})
		return
	}
	children := make([]string, 0)
	for _, s := range l.store.Sites() {
		children = append(children, s+"/")
	}
	if serveRootDoc(w, r, trimmed, l.RepoName, l.store.latestTimestamp(), children) {
		return
	}
	site, rest, _ := strings.Cut(trimmed, "/")
	rel, ok := cleanRel(rest)
	if !validSite(site) || !ok || strings.HasSuffix(r.URL.Path, "/") {
		pkgbase.NotFound(w)
		return
	}
	if !pkgbase.ServeStored(w, r, l.store.Store, l.store.siteRel(site, rel), contentType(rel)) {
		pkgbase.NotFound(w)
	}
}

func (l *Local) uploadZip(w http.ResponseWriter, r *http.Request, site string) {
	if !validSite(site) {
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /upload/{site} with a valid site name")
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "invalid zip: "+err.Error())
		return
	}
	max := l.MaxUpload
	if max <= 0 {
		max = pkgbase.DefaultMaxUpload
	}
	files, err := siteFiles(zr, 4*max)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	var size int64
	for _, f := range files {
		size += int64(f.f.UncompressedSize64)
	}
	exists := l.store.siteExists(site)
	if exists {
		_, old := l.store.Usage(l.store.siteRel(site))
		size -= old
	}
	if !l.AllowPublish(w, exists, max0(size)) {
		return
	}
	if _, _, errs := l.store.DeleteTree(l.store.siteRel(site)); len(errs) > 0 {
		pkgbase.Error(w, http.StatusInternalServerError, errs[0].Error())
		return
	}
	var total int64
	for _, f := range files {
		rc, err := f.f.Open()
		if err != nil {
			pkgbase.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		data, err := io.ReadAll(io.LimitReader(rc, int64(f.f.UncompressedSize64)+1))
		rc.Close()
		if err != nil {
			pkgbase.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := l.store.Write(l.store.siteRel(site, f.rel), data); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		total += int64(len(data))
	}
	if err := l.store.touch(site, time.Now()); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(site, total)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"site": site, "version": l.store.siteVersion(site), "files": len(files), "size": total,
		"location": pkgbase.PublicBase(r) + "/" + site + "/",
	})
}

func max0(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

func (l *Local) uploadFile(w http.ResponseWriter, r *http.Request, trimmed string) {
	site, rest, _ := strings.Cut(trimmed, "/")
	rel, ok := cleanRel(rest)
	if !validSite(site) || !ok {
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /upload/{site} (zip) or PUT /{site}/{path}")
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	target := l.store.siteRel(site, rel)
	if !l.AllowPublish(w, l.store.Exists(target), int64(len(body))) {
		return
	}
	if err := l.store.Write(target, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.touch(site, time.Now()); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(site+"/"+rel, int64(len(body)))
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{"site": site, "path": rel, "size": len(body), "sha256": pkgbase.SHA256Hex(body)})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request, trimmed string) {
	site, rest, hasRest := strings.Cut(trimmed, "/")
	if !hasRest || rest == "" {
		if err := l.DeleteVersion(r.Context(), site, ""); err != nil {
			if pkgbase.IsNotFound(err) {
				pkgbase.NotFound(w)
				return
			}
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rel, ok := cleanRel(rest)
	if !validSite(site) || !ok || !l.store.Exists(l.store.siteRel(site, rel)) {
		pkgbase.NotFound(w)
		return
	}
	if err := l.store.Delete(l.store.siteRel(site, rel)); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !l.store.siteExists(site) {
		_ = l.store.Delete(l.store.siteMetaRel(site))
	}
	l.EmitDeleted(site + "/" + rel)
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/p2.index")
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
	return bundleDetail(rr.store, name)
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, metaDir), nil
}

// ClassifyRequest maps plugin/feature downloads to {bundle id, version}.
func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	id, v, ok := bundleOf(r.URL.Path)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: id, Version: v}, true
}

func (rr *Remote) cacheRel(trimmed string, dir bool) string {
	switch {
	case trimmed == "":
		return path.Join(metaDir, "_index")
	case dir:
		return path.Join(metaDir, trimmed, "_index")
	case isArtifactPath(trimmed):
		return path.Join(filesDir, trimmed)
	}
	return path.Join(metaDir, trimmed)
}

// Prefetch warms the root metadata of the (sub-)site name ("" = the
// upstream root). version is ignored: p2 sites are not versioned.
func (rr *Remote) Prefetch(ctx context.Context, name, _ string) error {
	prefix := strings.Trim(name, "/")
	if prefix != "" {
		if _, ok := cleanRel(prefix); !ok {
			return registry.ErrInvalidPackageName
		}
		prefix += "/"
	}
	var got bool
	var lastErr error
	for _, f := range []string{"p2.index", "compositeContent.xml", "compositeArtifacts.xml", "content.jar", "artifacts.jar", "content.xml", "artifacts.xml"} {
		_, err := rr.FetchCached(ctx, rr.cacheRel(prefix+f, false), "/"+prefix+f, true)
		if err == nil {
			got = true
		} else {
			lastErr = err
		}
	}
	if !got {
		return lastErr
	}
	return nil
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	trimmed := strings.Trim(r.URL.Path, "/")
	dir := strings.HasSuffix(r.URL.Path, "/") && trimmed != ""
	if trimmed != "" {
		if _, ok := cleanRel(trimmed); !ok {
			pkgbase.NotFound(w)
			return
		}
	}
	upath := "/" + trimmed
	if dir || trimmed == "" {
		upath += "/"
	}
	rel := rr.cacheRel(trimmed, dir)
	if isArtifactPath(trimmed) && !dir {
		if pkgbase.ServeStored(w, r, rr.store.Store, rel, contentType(trimmed)) {
			return
		}
		rr.ServeCached(w, r, rel, upath, contentType(trimmed), false)
		return
	}
	body, err := rr.FetchCached(r.Context(), rel, upath, true)
	if err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	ct := contentType(trimmed)
	if dir || trimmed == "" {
		ct = "text/html; charset=utf-8"
	}
	if strings.HasSuffix(trimmed, ".xml") || dir || trimmed == "" {
		up := strings.TrimRight(rr.Client.BaseURL(), "/") + "/"
		body = bytes.ReplaceAll(body, []byte(up), []byte(pkgbase.PublicBase(r)+"/"))
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, ct, body)
}

// ── Virtual ──

// Virtual exposes a composite over its members, each reachable as the
// child "{member}/".
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
	trimmed := strings.Trim(r.URL.Path, "/")
	members := v.Members()
	if trimmed == "" {
		out := make([]map[string]string, 0, len(members))
		for _, m := range members {
			out = append(out, map[string]string{"name": m, "location": m + "/"})
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"sites": out})
		return
	}
	children := make([]string, 0, len(members))
	for _, m := range members {
		children = append(children, m+"/")
	}
	if serveRootDoc(w, r, trimmed, v.Name(), 0, children) {
		return
	}
	member, rest, _ := strings.Cut(trimmed, "/")
	isMember := false
	for _, m := range members {
		if m == member {
			isMember = true
			break
		}
	}
	reg, ok := v.Resolve(member)
	if !isMember || !ok {
		pkgbase.NotFound(w)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + rest
	if strings.HasSuffix(r.URL.Path, "/") && rest != "" {
		r2.URL.Path += "/"
	}
	r2.URL.RawPath = ""
	r2.Header.Set("X-Pika-Registry-Prefix", pkgbase.Prefix(r)+"/"+member)
	if code, msg, ok := registry.CheckGate(reg, r2); !ok {
		pkgbase.Error(w, code, msg)
		return
	}
	reg.ServeHTTP(w, r2)
}
