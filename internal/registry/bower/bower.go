// Package bower implements the Bower registry API:
//
//	GET    /packages                         [{name,url}] every registered package
//	GET    /packages/{name}                  {name,url}
//	GET    /packages/search/{q}              [{name,url}] substring match
//	POST   /packages                         register (form/JSON: name, url) (local, allow_push)
//	DELETE /packages/{name}                  unregister (local)
//	PUT    /archives/{name}/{version}        upload a .tar.gz (local, allow_push)
//	GET    /archives/{name}/{version}.tar.gz hosted archive
//	GET    /archives/{name}/latest.tar.gz    newest hosted archive
//	DELETE /archives/{name}/{version}        delete a hosted archive (local)
//
// Registry entries normally point at git URLs; Bower resolves
// versions from the git tags, so kutu only stores the name → url
// mapping. Hosted archives are a convenience: the first upload of a
// name registers it with url {base}/archives/{name}/latest.tar.gz.
// Bower treats archive URLs as a single, unversioned endpoint, so
// semver ranges cannot be resolved against hosted archives; pin a
// specific version with "name": "{base}/archives/{name}/{version}.tar.gz".
// Archive URLs carry no credentials, so the repo must be reachable
// by bower's downloader (e.g. embed x:<token>@ in the endpoint).
//
// Remote repos proxy https://registry.bower.io (entries and listings
// honour MutableTTL); virtual repos serve entries first-hit and union
// listings.
//
// Client configuration (.bowerrc):
//
//	{"registry": "https://x:<token>@kutu.example.com/registries/{ns}/{repo}"}
package bower

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeBower

// DefaultUpstream is used by remote repos that leave URL empty.
const DefaultUpstream = "https://registry.bower.io"

// baseToken is stored in hosted entries in place of the public base
// URL and substituted at serve time.
const baseToken = "{{kutu-base}}"

const (
	packagesDir = "packages"
	archivesDir = "archives"
	metaDir     = "meta"
	latestName  = "latest"
	ctTarGz     = "application/gzip"
)

// Entry is one registry record.
type Entry struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Created string `json:"created,omitempty"`
}

type archiveMeta struct {
	License     string `json:"license,omitempty"`
	Description string `json:"description,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	Published   string `json:"published,omitempty"`
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 214 {
		return false
	}
	for _, r := range s {
		if r == '/' || r == '\\' || r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

type reqKind int

const (
	reqOther reqKind = iota
	reqList
	reqEntry
	reqSearch
	reqArchive
	reqArchiveDir
)

type parsed struct {
	kind    reqKind
	name    string
	version string
	query   string
}

func parse(p string) parsed {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "packages":
		return parsed{kind: reqList}
	case len(parts) == 2 && parts[0] == "packages" && validName(parts[1]):
		return parsed{kind: reqEntry, name: parts[1]}
	case len(parts) == 3 && parts[0] == "packages" && parts[1] == "search":
		return parsed{kind: reqSearch, query: parts[2]}
	case len(parts) == 3 && parts[0] == "archives" && validName(parts[1]):
		ver := strings.TrimSuffix(parts[2], ".tar.gz")
		if validName(ver) {
			return parsed{kind: reqArchive, name: parts[1], version: ver}
		}
	case len(parts) == 2 && parts[0] == "archives" && validName(parts[1]):
		return parsed{kind: reqArchiveDir, name: parts[1]}
	}
	return parsed{}
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := parse(r.URL.Path)
	switch p.kind {
	case reqEntry, reqArchiveDir:
		return registry.ArtifactRef{Name: p.name}, true
	case reqArchive:
		if p.version == latestName {
			return registry.ArtifactRef{Name: p.name}, true
		}
		return registry.ArtifactRef{Name: p.name, Version: p.version}, true
	}
	return registry.ArtifactRef{}, false
}

func expand(e Entry, base string) Entry {
	e.URL = strings.ReplaceAll(e.URL, baseToken, base)
	return e
}

// ── Store ──

// Store wraps pkgbase.Store with the bower layout:
// packages/{name}.json and archives/{name}/{version}.tar.gz(+.json).
type Store struct{ *pkgbase.Store }

func (s *Store) entryRel(name string) string { return path.Join(packagesDir, name+".json") }
func (s *Store) archiveRel(name, ver string) string {
	return path.Join(archivesDir, name, ver+".tar.gz")
}
func (s *Store) archiveMetaRel(name, ver string) string {
	return path.Join(archivesDir, name, ver+".json")
}

// Entry returns the registry record of name.
func (s *Store) Entry(name string) (Entry, error) {
	var e Entry
	if !validName(name) {
		return e, registry.ErrPackageNotFound
	}
	b, err := s.Read(s.entryRel(name))
	if err != nil {
		return e, registry.ErrPackageNotFound
	}
	err = json.Unmarshal(b, &e)
	return e, err
}

// Entries returns every registry record, sorted by name.
func (s *Store) Entries() ([]Entry, error) {
	files, err := s.ListFiles(packagesDir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(files))
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		if e, err := s.Entry(strings.TrimSuffix(f.Name, ".json")); err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Store) putEntry(e Entry) error {
	b, err := pkgbase.MarshalJSON(e)
	if err != nil {
		return err
	}
	return s.Write(s.entryRel(e.Name), b)
}

// Versions returns the hosted archive versions of name (ascending).
func (s *Store) Versions(name string) []string {
	files, _ := s.ListFiles(path.Join(archivesDir, name))
	var out []string
	for _, f := range files {
		if v, ok := strings.CutSuffix(f.Name, ".tar.gz"); ok && v != latestName {
			out = append(out, v)
		}
	}
	pkgbase.SortVersions(out)
	return out
}

// ListPackages returns registered names plus hosted archive versions.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names := map[string]struct{}{}
	entries, err := s.Entries()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		names[e.Name] = struct{}{}
	}
	dirs, _ := s.ListDirs(archivesDir)
	for _, d := range dirs {
		names[d] = struct{}{}
	}
	out := make([]registry.PackageSummary, 0, len(names))
	for n := range names {
		vs := s.Versions(n)
		if len(vs) == 0 && !s.Exists(s.entryRel(n)) {
			continue
		}
		out = append(out, registry.PackageSummary{Name: n, Versions: vs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) readMeta(name, ver string) archiveMeta {
	var m archiveMeta
	if b, err := s.Read(s.archiveMetaRel(name, ver)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func isHosted(e Entry) bool { return strings.HasPrefix(e.URL, baseToken) }

// DeleteVersion removes one hosted archive; version "" or "*" removes
// the registration and every archive. A hosted registration is dropped
// together with its last archive.
func (s *Store) DeleteVersion(_ context.Context, name, version string) error {
	if !validName(name) {
		return registry.ErrPackageNotFound
	}
	if version == "" || version == "*" {
		hasEntry := s.Exists(s.entryRel(name))
		vs := s.Versions(name)
		if !hasEntry && len(vs) == 0 {
			return registry.ErrPackageNotFound
		}
		if _, _, errs := s.DeleteTree(path.Join(archivesDir, name)); len(errs) > 0 {
			return errs[0]
		}
		return s.Delete(s.entryRel(name))
	}
	if !validName(version) || version == latestName || !s.Exists(s.archiveRel(name, version)) {
		return registry.ErrPackageNotFound
	}
	if err := s.Delete(s.archiveRel(name, version)); err != nil {
		return err
	}
	if err := s.Delete(s.archiveMetaRel(name, version)); err != nil {
		return err
	}
	if len(s.Versions(name)) == 0 {
		if e, err := s.Entry(name); err == nil && isHosted(e) {
			return s.Delete(s.entryRel(name))
		}
	}
	return nil
}

func detail(s *Store, name string, hostedBase string) (*registry.PackageDetail, error) {
	e, eerr := s.Entry(name)
	versions := s.Versions(name)
	if eerr != nil && len(versions) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d := &registry.GenericPackageDetail{LatestVersion: pkgbase.Latest(versions), Metadata: map[string]string{}}
	if eerr == nil {
		d.Metadata["url"] = expand(e, hostedBase).URL
	}
	if d.LatestVersion != "" {
		m := s.readMeta(name, d.LatestVersion)
		d.Description, d.Homepage, d.License = m.Description, m.Homepage, m.License
	}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		row := registry.GenericVersionDetail{Version: v, PublishedAt: s.readMeta(name, v).Published}
		if fi, err := s.Stat(s.archiveRel(name, v)); err == nil {
			row.Size = fi.Size
			row.Files = []registry.GenericFile{{Name: v + ".tar.gz", Size: fi.Size}}
			if row.PublishedAt == "" {
				row.PublishedAt = fi.ModTime.UTC().Format(time.RFC3339)
			}
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

// serveArchive serves name@version (or the newest for "latest").
func serveArchive(w http.ResponseWriter, r *http.Request, s *Store, name, ver string) bool {
	if ver == latestName {
		ver = pkgbase.Latest(s.Versions(name))
		if ver == "" {
			return false
		}
	}
	return pkgbase.ServeStored(w, r, s.Store, s.archiveRel(name, ver), ctTarGz)
}

func search(entries []Entry, q string) []Entry {
	q = strings.ToLower(q)
	out := []Entry{}
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name), q) {
			out = append(out, e)
		}
	}
	return out
}

// bowerJSON extracts the shallowest bower.json from a .tar.gz.
func bowerJSON(tgz []byte) (map[string]any, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var best []byte
	bestDepth := -1
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "bower.json" {
			continue
		}
		depth := strings.Count(strings.Trim(path.Clean(h.Name), "/"), "/")
		if bestDepth >= 0 && depth >= bestDepth {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, err
		}
		best, bestDepth = b, depth
	}
	if best == nil {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(best, &m); err != nil {
		return nil, fmt.Errorf("bower.json: %w", err)
	}
	return m, nil
}

func licenseOf(v any) string {
	switch l := v.(type) {
	case string:
		return l
	case []any:
		var parts []string
		for _, x := range l {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " OR ")
	}
	return ""
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
	return detail(l.store, name, "")
}

func (l *Local) DeleteVersion(ctx context.Context, name, version string) error {
	if err := l.store.DeleteVersion(ctx, name, version); err != nil {
		return err
	}
	if version == "" || version == "*" {
		l.EmitDeleted(name)
	} else {
		l.EmitDeleted(name + "@" + version)
	}
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if !validName(ref.Name) || !validName(ref.Version) {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	fi, err := l.store.Stat(l.store.archiveRel(ref.Name, ref.Version))
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	m := l.store.readMeta(ref.Name, ref.Version)
	out := registry.ArtifactMeta{License: m.License, PublishedAt: fi.ModTime}
	if t, err := time.Parse(time.RFC3339, m.Published); err == nil {
		out.PublishedAt = t
	}
	return out, nil
}

// PromoteVersion copies the registration of name and, when version is
// set, the hosted archive into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("bower: promote target %s/%s is not a bower local repository", dst.Namespace(), dst.Name())
	}
	e, eerr := l.store.Entry(name)
	if version == "" || version == "*" {
		if eerr != nil {
			return registry.ErrPackageNotFound
		}
		if err := d.store.putEntry(e); err != nil {
			return err
		}
		d.EmitPublished(name, 0)
		return nil
	}
	if !validName(version) {
		return registry.ErrPackageNotFound
	}
	body, err := l.store.Read(l.store.archiveRel(name, version))
	if err != nil {
		return registry.ErrPackageNotFound
	}
	if _, err := d.Guard.Check(d.store.Store, d.store.Exists(d.store.archiveRel(name, version)), int64(len(body))); err != nil {
		return err
	}
	if err := d.store.Write(d.store.archiveRel(name, version), body); err != nil {
		return err
	}
	if meta, err := l.store.Read(l.store.archiveMetaRel(name, version)); err == nil {
		if err := d.store.Write(d.store.archiveMetaRel(name, version), meta); err != nil {
			return err
		}
	}
	if !d.store.Exists(d.store.entryRel(name)) {
		if eerr != nil {
			e = Entry{Name: name, URL: baseToken + "/archives/" + name + "/latest.tar.gz", Created: time.Now().UTC().Format(time.RFC3339)}
		}
		if err := d.store.putEntry(e); err != nil {
			return err
		}
	}
	d.EmitPublished(name+"@"+version, int64(len(body)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := parse(r.URL.Path)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveRead(w, r, l.store, p)
	case http.MethodPost:
		if p.kind != reqList {
			pkgbase.Error(w, http.StatusBadRequest, "expected POST /packages")
			return
		}
		l.register(w, r)
	case http.MethodPut:
		if p.kind != reqArchive || p.version == latestName {
			pkgbase.Error(w, http.StatusBadRequest, "expected PUT /archives/{name}/{version}")
			return
		}
		l.upload(w, r, p.name, p.version)
	case http.MethodDelete:
		if !l.CheckPush(w) {
			return
		}
		var err error
		switch p.kind {
		case reqEntry:
			err = l.DeleteVersion(r.Context(), p.name, "*")
		case reqArchive:
			err = l.DeleteVersion(r.Context(), p.name, p.version)
		default:
			err = registry.ErrPackageNotFound
		}
		if err != nil {
			if pkgbase.IsNotFound(err) {
				pkgbase.NotFound(w)
				return
			}
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func serveRead(w http.ResponseWriter, r *http.Request, s *Store, p parsed) {
	base := pkgbase.PublicBase(r)
	switch p.kind {
	case reqList, reqSearch:
		entries, err := s.Entries()
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if p.kind == reqSearch {
			entries = search(entries, p.query)
		}
		for i := range entries {
			entries[i] = expand(entries[i], base)
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, entries)
	case reqEntry:
		e, err := s.Entry(p.name)
		if err != nil {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, expand(e, base))
	case reqArchiveDir:
		vs := s.Versions(p.name)
		if len(vs) == 0 {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"name": p.name, "versions": vs, "latest": pkgbase.Latest(vs)})
	case reqArchive:
		if !serveArchive(w, r, s, p.name, p.version) {
			pkgbase.NotFound(w)
		}
	default:
		pkgbase.NotFound(w)
	}
}

func (l *Local) register(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	var e Entry
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		body, err := pkgbase.ReadBody(r.Body, 1<<20)
		if err == nil {
			err = json.Unmarshal(body, &e)
		}
		if err != nil {
			pkgbase.Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			pkgbase.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		e.Name, e.URL = r.PostForm.Get("name"), r.PostForm.Get("url")
	}
	e.Name, e.URL = strings.TrimSpace(e.Name), strings.TrimSpace(e.URL)
	if !validName(e.Name) || e.URL == "" {
		pkgbase.Error(w, http.StatusBadRequest, "name and url are required")
		return
	}
	if old, err := l.store.Entry(e.Name); err == nil {
		if old.URL == e.URL {
			pkgbase.WriteJSON(w, r, http.StatusOK, old)
			return
		}
		pkgbase.Error(w, http.StatusForbidden, "package already registered")
		return
	}
	if !l.AllowPublish(w, false, int64(len(e.URL))) {
		return
	}
	e.Created = time.Now().UTC().Format(time.RFC3339)
	if err := l.store.putEntry(e); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(e.Name, 0)
	pkgbase.WriteJSON(w, r, http.StatusCreated, e)
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request, name, ver string) {
	if !l.CheckPush(w) {
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	bj, err := bowerJSON(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "invalid .tar.gz: "+err.Error())
		return
	}
	if !l.AllowPublish(w, l.store.Exists(l.store.archiveRel(name, ver)), int64(len(body))) {
		return
	}
	meta := archiveMeta{Published: time.Now().UTC().Format(time.RFC3339)}
	if bj != nil {
		meta.License = licenseOf(bj["license"])
		meta.Description, _ = bj["description"].(string)
		meta.Homepage, _ = bj["homepage"].(string)
	}
	mb, _ := pkgbase.MarshalJSON(meta)
	if err := l.store.Write(l.store.archiveRel(name, ver), body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.Write(l.store.archiveMetaRel(name, ver), mb); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := l.store.Entry(name); err != nil {
		e := Entry{Name: name, URL: baseToken + "/archives/" + name + "/latest.tar.gz", Created: meta.Published}
		if err := l.store.putEntry(e); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	l.EmitPublished(name+"@"+ver, int64(len(body)))
	base := pkgbase.PublicBase(r)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"name": name, "version": ver, "size": len(body), "sha256": pkgbase.SHA256Hex(body),
		"url": base + "/archives/" + name + "/" + ver + ".tar.gz",
	})
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		if r.URL == "" {
			cp := *r
			cp.URL = DefaultUpstream
			r = &cp
		}
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/packages/jquery")
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
	return detail(rr.store, name, "")
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, metaDir, packagesDir), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// rewrite points archive URLs hosted by an upstream kutu back at this repo.
func (rr *Remote) rewrite(body []byte, base string) []byte {
	up := strings.TrimRight(rr.Client.BaseURL(), "/") + "/archives/"
	return bytes.ReplaceAll(body, []byte(up), []byte(base+"/archives/"))
}

func (rr *Remote) fetchEntry(ctx context.Context, name string) (Entry, error) {
	var e Entry
	body, err := rr.FetchCached(ctx, rr.store.entryRel(name), "/packages/"+name, true)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return e, err
	}
	return e, nil
}

// Prefetch warms the registry entry and, when given, a hosted archive.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if !validName(name) {
		return registry.ErrInvalidPackageName
	}
	if _, err := rr.fetchEntry(ctx, name); err != nil {
		return err
	}
	if version == "" || !validName(version) {
		return nil
	}
	_, err := rr.FetchCached(ctx, rr.store.archiveRel(name, version), "/archives/"+name+"/"+version+".tar.gz", false)
	return err
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	base := pkgbase.PublicBase(r)
	p := parse(r.URL.Path)
	var (
		body []byte
		err  error
	)
	switch p.kind {
	case reqList:
		body, err = rr.FetchCached(r.Context(), path.Join(metaDir, "packages.json"), "/packages", true)
	case reqSearch:
		body, err = rr.FetchCached(r.Context(), path.Join(metaDir, "search", pkgbase.SHA256Hex([]byte(p.query))+".json"), "/packages/search/"+p.query, true)
	case reqEntry:
		body, err = rr.FetchCached(r.Context(), rr.store.entryRel(p.name), "/packages/"+p.name, true)
	case reqArchiveDir:
		body, err = rr.FetchCached(r.Context(), path.Join(metaDir, archivesDir, p.name+".json"), "/archives/"+p.name, true)
	case reqArchive:
		rel := rr.store.archiveRel(p.name, p.version)
		upath := "/archives/" + p.name + "/" + p.version + ".tar.gz"
		if p.version == latestName {
			rr.ServeCached(w, r, path.Join(metaDir, archivesDir, p.name, latestName+".tar.gz"), upath, ctTarGz, true)
			return
		}
		if pkgbase.ServeStored(w, r, rr.store.Store, rel, ctTarGz) {
			return
		}
		rr.ServeCached(w, r, rel, upath, ctTarGz, false)
		return
	default:
		pkgbase.NotFound(w)
		return
	}
	if err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", rr.rewrite(body, base))
}

// ── Virtual ──

// Virtual serves entries/archives first-hit and unions listings.
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
	switch parse(r.URL.Path).kind {
	case reqList, reqSearch:
		seen := map[string]struct{}{}
		out := []Entry{}
		for _, b := range v.CollectMembers(r) {
			var entries []Entry
			if json.Unmarshal(b, &entries) != nil {
				continue
			}
			for _, e := range entries {
				if _, dup := seen[e.Name]; dup {
					continue
				}
				seen[e.Name] = struct{}{}
				out = append(out, e)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		pkgbase.WriteJSON(w, r, http.StatusOK, out)
	default:
		if !v.ServeFirstHit(w, r) {
			pkgbase.NotFound(w)
		}
	}
}
