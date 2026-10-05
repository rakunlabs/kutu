// Package nuget implements a NuGet V3 feed usable by `dotnet nuget`,
// nuget.exe and Chocolatey 2.x:
//
//	GET    /v3/index.json  (also /index.json, /)                service index
//	GET    /v3/flatcontainer/{id}/index.json                    version list (lowercase)
//	GET    /v3/flatcontainer/{id}/{ver}/{id}.{ver}.nupkg        package download
//	GET    /v3/flatcontainer/{id}/{ver}/{id}.nuspec             manifest
//	GET    /v3/registration/{id}/index.json                     registration index (inlined page)
//	GET    /v3/registration/{id}/{ver}.json                     registration leaf
//	GET    /v3/search?q=&skip=&take=&prerelease=                search (substring match)
//	GET    /v3/autocomplete?q= | ?id=                           autocomplete
//	PUT    /api/v2/package                                      push (multipart nupkg, local)
//	DELETE /api/v2/package/{id}/{ver}                           hard delete (local)
//	POST   /api/v2/package/{id}/{ver}                           relist (local)
//
// IDs and versions in URLs are lowercased and normalized (build
// metadata stripped, 1.0 → 1.0.0, 1.0.0.0 → 1.0.0). DELETE removes the
// version entirely instead of unlisting it; unlisted state only arises
// from metadata copied from an upstream.
//
// Remote repos proxy any NuGet V3 feed (URL may be the service index,
// e.g. https://api.nuget.org/v3/index.json, or the host root). The
// upstream service index is used to discover PackageBaseAddress,
// RegistrationsBaseUrl and SearchQueryService; every upstream URL in
// proxied documents is rewritten to point back at kutu. Packages are
// cached forever, version lists and registrations honour MutableTTL.
// Virtual repos merge version lists, registrations, search and
// autocomplete across members; everything else is first-hit.
//
// Client configuration:
//
//	dotnet nuget add source https://kutu.example.com/registries/{ns}/{repo}/v3/index.json \
//	  -n kutu -u x -p <token> --store-password-in-clear-text
//	dotnet nuget push pkg.nupkg -s kutu -k <token>
package nuget

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeNuGet

// packagesDir holds {lid}/{lver}/{lid}.{lver}.nupkg, {lid}.nuspec and meta.json.
const packagesDir = "packages"

// Store wraps pkgbase.Store with the NuGet layout.
type Store struct{ *pkgbase.Store }

func (s *Store) versionDir(lid, lver string) string { return path.Join(packagesDir, lid, lver) }

func (s *Store) nupkgRel(lid, lver string) string {
	return path.Join(packagesDir, lid, lver, lid+"."+lver+".nupkg")
}

func (s *Store) nuspecRel(lid, lver string) string {
	return path.Join(packagesDir, lid, lver, lid+".nuspec")
}

func (s *Store) metaRel(lid, lver string) string {
	return path.Join(packagesDir, lid, lver, "meta.json")
}

// ReadMeta returns the stored metadata of lid@lver.
func (s *Store) ReadMeta(lid, lver string) (*Meta, error) {
	b, err := s.Read(s.metaRel(lid, lver))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, registry.ErrPackageNotFound
		}
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) writeMeta(m *Meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.Write(s.metaRel(strings.ToLower(m.ID), strings.ToLower(m.Version)), b)
}

// Versions returns every stored version of lid (ascending).
func (s *Store) Versions(lid string) ([]*Meta, error) {
	dirs, err := s.ListDirs(path.Join(packagesDir, lid))
	if err != nil {
		return nil, err
	}
	var out []*Meta
	for _, d := range dirs {
		m, err := s.ReadMeta(lid, d)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return pkgbase.CompareVersions(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

func (s *Store) all() ([]pkgVersions, error) {
	ids, err := s.ListDirs(packagesDir)
	if err != nil {
		return nil, err
	}
	var out []pkgVersions
	for _, lid := range ids {
		metas, _ := s.Versions(lid)
		if len(metas) == 0 {
			continue
		}
		out = append(out, pkgVersions{ID: metas[len(metas)-1].ID, Metas: metas})
	}
	return out, nil
}

// ListPackages returns every package id with its versions.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	pkgs, err := s.all()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(pkgs))
	for _, p := range pkgs {
		vs := make([]string, 0, len(p.Metas))
		for _, m := range p.Metas {
			vs = append(vs, m.Version)
		}
		out = append(out, registry.PackageSummary{Name: p.ID, Versions: vs})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// DeleteVersion removes every file of name@version.
func (s *Store) DeleteVersion(_ context.Context, name, version string) error {
	lid := strings.ToLower(name)
	lver, ok := lowerVersion(version)
	if !ok || !ValidID(name) {
		return registry.ErrPackageNotFound
	}
	files, err := s.ListFiles(s.versionDir(lid, lver))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, f := range files {
		if err := s.Delete(path.Join(s.versionDir(lid, lver), f.Name)); err != nil {
			return err
		}
	}
	return nil
}

// storeNupkg writes nupkg + extracted nuspec + meta.json. published
// may be zero. When lid/lver are non-empty (remote cache) the files are
// stored under them and an unparseable nuspec is tolerated.
func (s *Store) storeNupkg(nupkg []byte, published time.Time, lid, lver string) (*Meta, error) {
	raw, err := ExtractNuspec(nupkg)
	var m *Meta
	if err == nil {
		m, err = ParseNuspec(raw)
	}
	if err != nil {
		if lid == "" {
			return nil, err
		}
		m = &Meta{ID: lid, Version: lver, Listed: true}
	}
	m.Published = published
	m.Size = int64(len(nupkg))
	sum := sha512.Sum512(nupkg)
	m.SHA512 = base64.StdEncoding.EncodeToString(sum[:])
	if lid == "" {
		lid, lver = strings.ToLower(m.ID), strings.ToLower(m.Version)
	}
	if err := s.Write(s.nupkgRel(lid, lver), nupkg); err != nil {
		return nil, err
	}
	if raw != nil {
		if err := s.Write(s.nuspecRel(lid, lver), raw); err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err := s.Write(s.metaRel(lid, lver), b); err != nil {
		return nil, err
	}
	return m, nil
}

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	lid := strings.ToLower(strings.TrimSpace(name))
	if !ValidID(lid) {
		return nil, registry.ErrInvalidPackageName
	}
	metas, err := s.Versions(lid)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	latest := metas[len(metas)-1]
	vs := make([]string, 0, len(metas))
	for _, m := range metas {
		vs = append(vs, m.Version)
	}
	d := &registry.GenericPackageDetail{
		LatestVersion: pkgbase.Latest(vs),
		Description:   latest.Description,
		Homepage:      latest.ProjectURL,
		License:       latest.License(),
		Metadata:      map[string]string{},
	}
	if latest.Authors != "" {
		d.Metadata["authors"] = latest.Authors
	}
	if len(latest.Tags) > 0 {
		d.Metadata["tags"] = strings.Join(latest.Tags, " ")
	}
	if latest.Title != "" {
		d.Metadata["title"] = latest.Title
	}
	for i := len(metas) - 1; i >= 0; i-- {
		m := metas[i]
		lver := strings.ToLower(m.Version)
		row := registry.GenericVersionDetail{Version: m.Version, Size: m.Size, Yanked: !m.Listed}
		if !m.Published.IsZero() {
			row.PublishedAt = m.Published.UTC().Format(time.RFC3339)
		}
		files, _ := s.ListFiles(s.versionDir(lid, lver))
		for _, f := range files {
			if f.Name == "meta.json" {
				continue
			}
			row.Files = append(row.Files, registry.GenericFile{Name: f.Name, Size: f.Size})
		}
		if lic := m.License(); lic != "" {
			row.Metadata = map[string]string{"license": lic}
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: latest.ID, Generic: d}, nil
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	lver, ok := lowerVersion(ref.Version)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	m, err := s.ReadMeta(strings.ToLower(ref.Name), lver)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	return registry.ArtifactMeta{License: m.License(), PublishedAt: m.Published}, nil
}

// classify maps flat-container / registration requests to a package.
func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, flatPrefix):
		parts := splitFlat(p)
		if len(parts) == 0 || !ValidID(parts[0]) {
			return registry.ArtifactRef{}, false
		}
		ref := registry.ArtifactRef{Name: strings.ToLower(parts[0])}
		if len(parts) == 3 && strings.HasSuffix(strings.ToLower(parts[2]), ".nupkg") {
			if lver, ok := lowerVersion(parts[1]); ok {
				ref.Version = lver
			}
		}
		return ref, true
	case strings.HasPrefix(p, regPrefix):
		rest := strings.Trim(strings.TrimPrefix(p, regPrefix), "/")
		id, _, _ := strings.Cut(rest, "/")
		if !ValidID(id) {
			return registry.ArtifactRef{}, false
		}
		return registry.ArtifactRef{Name: strings.ToLower(id)}, true
	case strings.HasPrefix(p, publishPath+"/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, publishPath+"/"), "/")
		if !ValidID(id) {
			return registry.ArtifactRef{}, false
		}
		return registry.ArtifactRef{Name: strings.ToLower(id)}, true
	}
	return registry.ArtifactRef{}, false
}

// serveRead serves the read side of a store-backed feed.
func serveRead(w http.ResponseWriter, r *http.Request, s *Store) {
	p := r.URL.Path
	base := pkgbase.PublicBase(r)
	switch {
	case isServiceIndex(p):
		serveServiceIndex(w, r)
	case strings.HasPrefix(p, flatPrefix):
		serveFlat(w, r, s)
	case strings.HasPrefix(p, regPrefix):
		serveRegistration(w, r, s, base)
	case strings.TrimRight(p, "/") == searchPath:
		pkgs, err := s.all()
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, searchResponse(base, pkgs, r.URL.Query()))
	case strings.TrimRight(p, "/") == autocompletePath:
		pkgs, err := s.all()
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, autocompleteResponse(pkgs, r.URL.Query()))
	default:
		pkgbase.NotFound(w)
	}
}

func serveFlat(w http.ResponseWriter, r *http.Request, s *Store) {
	parts := splitFlat(r.URL.Path)
	if len(parts) == 0 || !ValidID(parts[0]) {
		pkgbase.NotFound(w)
		return
	}
	lid := strings.ToLower(parts[0])
	switch {
	case len(parts) == 2 && parts[1] == "index.json":
		metas, err := s.Versions(lid)
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(metas) == 0 {
			pkgbase.NotFound(w)
			return
		}
		vs := make([]string, 0, len(metas))
		for _, m := range metas {
			vs = append(vs, strings.ToLower(m.Version))
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"versions": vs})
	case len(parts) == 3:
		lver, ok := lowerVersion(parts[1])
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		file := strings.ToLower(parts[2])
		switch {
		case strings.HasSuffix(file, ".nupkg"):
			if pkgbase.ServeStored(w, r, s.Store, s.nupkgRel(lid, lver), "application/octet-stream") {
				return
			}
		case strings.HasSuffix(file, ".nuspec"):
			if pkgbase.ServeStored(w, r, s.Store, s.nuspecRel(lid, lver), "application/xml") {
				return
			}
		}
		pkgbase.NotFound(w)
	default:
		pkgbase.NotFound(w)
	}
}

func serveRegistration(w http.ResponseWriter, r *http.Request, s *Store, base string) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, regPrefix), "/")
	id, file, ok := strings.Cut(rest, "/")
	if !ok || !ValidID(id) || strings.Contains(file, "/") {
		pkgbase.NotFound(w)
		return
	}
	lid := strings.ToLower(id)
	if file == "index.json" {
		metas, err := s.Versions(lid)
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(metas) == 0 {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, registrationIndex(base, metas))
		return
	}
	ver, isJSON := strings.CutSuffix(file, ".json")
	lver, vok := lowerVersion(ver)
	if !isJSON || !vok {
		pkgbase.NotFound(w)
		return
	}
	m, err := s.ReadMeta(lid, lver)
	if err != nil {
		pkgbase.NotFound(w)
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, registrationLeaf(base, m))
}

// ── Local ──

// Local is a hosted NuGet feed.
type Local struct {
	*pkgbase.Repo
	store *Store
}

// NewLocalFactory returns the local NuGet factory.
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

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref)
}

// PromoteVersion copies name@version (nupkg, nuspec, metadata) into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("nuget: promote target %s/%s is not a local nuget repository", dst.Namespace(), dst.Name())
	}
	lid := strings.ToLower(name)
	lver, vok := lowerVersion(version)
	if !vok || !ValidID(name) {
		return registry.ErrPackageNotFound
	}
	if !l.store.Exists(l.store.metaRel(lid, lver)) {
		return registry.ErrPackageNotFound
	}
	nupkg, err := l.store.Read(l.store.nupkgRel(lid, lver))
	if err != nil {
		return err
	}
	if code, err := d.Guard.Check(d.store.Store, d.store.Exists(d.store.metaRel(lid, lver)), int64(len(nupkg))); code != 0 {
		return err
	}
	for _, rel := range []string{l.store.nupkgRel(lid, lver), l.store.nuspecRel(lid, lver), l.store.metaRel(lid, lver)} {
		b, err := l.store.Read(rel)
		if err != nil {
			return err
		}
		if err := d.store.Write(rel, b); err != nil {
			return err
		}
	}
	d.EmitPublished(lid+"@"+lver, int64(len(nupkg)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveRead(w, r, l.store)
	case http.MethodPut:
		switch strings.TrimRight(p, "/") {
		case publishPath, "", "/api/v2", "/v3":
			l.push(w, r)
		default:
			pkgbase.NotFound(w)
		}
	case http.MethodDelete:
		l.remove(w, r)
	case http.MethodPost:
		l.relist(w, r)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// readPushBody returns the nupkg from a multipart form (first part)
// or the raw body.
func (l *Local) readPushBody(r *http.Request) ([]byte, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(mt, "multipart/") {
		return pkgbase.ReadBody(r.Body, l.MaxUpload)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil, fmt.Errorf("multipart body contains no package")
		}
		if err != nil {
			return nil, err
		}
		body, err := pkgbase.ReadBody(part, l.MaxUpload)
		_ = part.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			return body, nil
		}
	}
}

func (l *Local) push(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	body, err := l.readPushBody(r)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "exceeds") {
			code = http.StatusRequestEntityTooLarge
		}
		pkgbase.Error(w, code, err.Error())
		return
	}
	raw, err := ExtractNuspec(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := ParseNuspec(raw)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	lid, lver := strings.ToLower(m.ID), strings.ToLower(m.Version)
	if !l.AllowPublish(w, l.store.Exists(l.store.metaRel(lid, lver)), int64(len(body))) {
		return
	}
	if _, err := l.store.storeNupkg(body, time.Now().UTC(), "", ""); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(m.ID+"@"+m.Version, int64(len(body)))
	w.WriteHeader(http.StatusCreated)
}

func idVersionFromPublish(p string) (string, string, bool) {
	rest, ok := strings.CutPrefix(p, publishPath+"/")
	if !ok {
		return "", "", false
	}
	id, ver, ok := strings.Cut(strings.Trim(rest, "/"), "/")
	if !ok || !ValidID(id) || strings.Contains(ver, "/") {
		return "", "", false
	}
	lver, vok := lowerVersion(ver)
	if !vok {
		return "", "", false
	}
	return strings.ToLower(id), lver, true
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	lid, lver, ok := idVersionFromPublish(r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	if err := l.store.DeleteVersion(r.Context(), lid, lver); err != nil {
		if pkgbase.IsNotFound(err) {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitDeleted(lid + "@" + lver)
	w.WriteHeader(http.StatusNoContent)
}

func (l *Local) relist(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	lid, lver, ok := idVersionFromPublish(r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	m, err := l.store.ReadMeta(lid, lver)
	if err != nil {
		pkgbase.NotFound(w)
		return
	}
	if !m.Listed {
		m.Listed = true
		if err := l.store.writeMeta(m); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
