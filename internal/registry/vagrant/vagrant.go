// Package vagrant implements a Vagrant box catalog (the metadata JSON
// protocol consumed by `vagrant box add <url>`):
//
//	GET    /{org}/{box}[.json]                         box metadata (versions/providers)
//	GET    /{org}/{box}/{version}/{provider}[/{arch}].box  box download
//	PUT    /{org}/{box}/{version}/{provider}[/{arch}]  upload a .box (sha256 computed)
//	DELETE /{org}/{box}/{version}[/{provider}[/{arch}]] delete a version / provider
//	GET    /api/v2/box/{org}/{box}                     Vagrant Cloud-style box info
//	GET    /api/v2/box/{org}/{box}/version/{version}   Vagrant Cloud-style version info
//	GET    /api/v2/search?q=                           box search
//
// Remote repos proxy an app.vagrantup.com-style catalog: metadata is
// fetched with Accept: application/json and honours MutableTTL,
// provider URLs are rewritten to kutu and .box files are cached
// forever on first download. Virtual repos merge versions.
//
// Client configuration:
//
//	vagrant box add https://x:<token>@kutu.example.com/registries/{ns}/{repo}/acme/devbox
//	# or, in a Vagrantfile:
//	config.vm.box     = "acme/devbox"
//	config.vm.box_url = "https://kutu.example.com/registries/{ns}/{repo}/acme/devbox"
//	config.vm.box_download_options = {"header" => "Authorization: Bearer <token>"}
//	# upload:
//	curl -u x:<token> -T dev.box https://kutu.example.com/registries/{ns}/{repo}/acme/devbox/1.0.0/virtualbox/amd64
package vagrant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeVagrant

// DefaultUpstream is the public Vagrant box catalog.
const DefaultUpstream = "https://app.vagrantup.com"

const (
	boxesDir     = "boxes"
	metaDir      = "meta"
	metadataFile = "metadata.json"
	defaultArch  = "_"
	ctBox        = "application/octet-stream"
)

// ── Metadata ──

// Metadata is the box catalog document.
type Metadata struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Versions    []Version `json:"versions"`
}

// Version is one box version.
type Version struct {
	Version     string     `json:"version"`
	Status      string     `json:"status,omitempty"`
	Description string     `json:"description_markdown,omitempty"`
	CreatedAt   string     `json:"created_at,omitempty"`
	Providers   []Provider `json:"providers"`
}

// Provider is one (provider, architecture) build of a version.
type Provider struct {
	Name                string `json:"name"`
	URL                 string `json:"url"`
	ChecksumType        string `json:"checksum_type,omitempty"`
	Checksum            string `json:"checksum,omitempty"`
	Architecture        string `json:"architecture,omitempty"`
	DefaultArchitecture bool   `json:"default_architecture,omitempty"`
	Size                int64  `json:"size,omitempty"`
	CreatedAt           string `json:"created_at,omitempty"`
}

func (p Provider) archKey() string {
	if p.Architecture == "" {
		return defaultArch
	}
	return p.Architecture
}

func sortVersions(vs []Version) {
	sort.SliceStable(vs, func(i, j int) bool { return pkgbase.CompareVersions(vs[i].Version, vs[j].Version) > 0 })
}

func findVersion(m *Metadata, v string) int {
	for i := range m.Versions {
		if m.Versions[i].Version == v {
			return i
		}
	}
	return -1
}

func findProvider(v *Version, name, arch string) int {
	for i, p := range v.Providers {
		if p.Name == name && p.archKey() == arch {
			return i
		}
	}
	return -1
}

func mergeMetadata(docs []Metadata) Metadata {
	var out Metadata
	for _, d := range docs {
		if out.Name == "" {
			out.Name = d.Name
		}
		if out.Description == "" {
			out.Description = d.Description
		}
		for _, v := range d.Versions {
			i := findVersion(&out, v.Version)
			if i < 0 {
				out.Versions = append(out.Versions, v)
				continue
			}
			for _, p := range v.Providers {
				if findProvider(&out.Versions[i], p.Name, p.archKey()) < 0 {
					out.Versions[i].Providers = append(out.Versions[i].Providers, p)
				}
			}
		}
	}
	if out.Versions == nil {
		out.Versions = []Version{}
	}
	sortVersions(out.Versions)
	return out
}

// ── Request parsing ──

type reqKind int

const (
	reqNone reqKind = iota
	reqMeta
	reqVersion
	reqProvider // PUT/DELETE target or extensionless download
	reqDownload
	reqAPIBox
	reqAPIVersion
	reqSearch
)

type request struct {
	kind                        reqKind
	org, box, version, provider string
	arch                        string
}

func (p request) name() string { return p.org + "/" + p.box }

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00?#")
}

func parse(p string) request {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for _, s := range parts {
		if !validSegment(s) {
			return request{}
		}
	}
	if len(parts) >= 2 && parts[0] == "api" && (parts[1] == "v1" || parts[1] == "v2") {
		rest := parts[2:]
		switch {
		case len(rest) == 1 && rest[0] == "search":
			return request{kind: reqSearch}
		case len(rest) == 3 && rest[0] == "box":
			return request{kind: reqAPIBox, org: rest[1], box: rest[2]}
		case len(rest) == 5 && rest[0] == "box" && rest[3] == "version":
			return request{kind: reqAPIVersion, org: rest[1], box: rest[2], version: rest[4]}
		}
		return request{}
	}
	switch len(parts) {
	case 2:
		return request{kind: reqMeta, org: parts[0], box: strings.TrimSuffix(parts[1], ".json")}
	case 3:
		return request{kind: reqVersion, org: parts[0], box: parts[1], version: parts[2]}
	case 4, 5:
		req := request{kind: reqProvider, org: parts[0], box: parts[1], version: parts[2], provider: parts[3], arch: defaultArch}
		if len(parts) == 5 {
			req.arch = parts[4]
		}
		last := &req.provider
		if len(parts) == 5 {
			last = &req.arch
		}
		if s, ok := strings.CutSuffix(*last, ".box"); ok && s != "" {
			*last = s
			req.kind = reqDownload
		}
		return req
	}
	return request{}
}

func boxURL(base, name, version, provider, arch string) string {
	u := base + "/" + name + "/" + version + "/" + provider
	if arch != defaultArch && arch != "" {
		return u + "/" + arch + ".box"
	}
	return u + ".box"
}

// ── Store ──

// Store wraps pkgbase.Store with the Vagrant layout:
//
//	boxes/{org}/{box}/metadata.json
//	boxes/{org}/{box}/{version}/{provider}/{arch}.box
type Store struct{ *pkgbase.Store }

func (s *Store) metaRel(name string) string { return path.Join(boxesDir, name, metadataFile) }

func (s *Store) boxRel(name, version, provider, arch string) string {
	return path.Join(boxesDir, name, version, provider, arch+".box")
}

func (s *Store) readMeta(name string) (Metadata, bool) {
	var m Metadata
	b, err := s.Read(s.metaRel(name))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return Metadata{Name: name}, false
	}
	return m, true
}

func (s *Store) writeMeta(m Metadata) error {
	if len(m.Versions) == 0 {
		return s.Delete(s.metaRel(m.Name))
	}
	sortVersions(m.Versions)
	b, err := pkgbase.MarshalJSON(m)
	if err != nil {
		return err
	}
	return s.Write(s.metaRel(m.Name), b)
}

func (s *Store) names() []string {
	var out []string
	orgs, _ := s.ListDirs(boxesDir)
	for _, o := range orgs {
		boxes, _ := s.ListDirs(path.Join(boxesDir, o))
		for _, b := range boxes {
			out = append(out, o+"/"+b)
		}
	}
	return out
}

// ListPackages returns "org/box" rows with their versions.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	var out []registry.PackageSummary
	for _, n := range s.names() {
		m, ok := s.readMeta(n)
		if !ok || len(m.Versions) == 0 {
			continue
		}
		vs := make([]string, 0, len(m.Versions))
		for _, v := range m.Versions {
			vs = append(vs, v.Version)
		}
		pkgbase.SortVersions(vs)
		out = append(out, registry.PackageSummary{Name: n, Versions: vs})
	}
	return out, nil
}

// deleteVersion removes version (or one provider/arch of it when
// provider != "") from the metadata and storage.
func (s *Store) deleteVersion(name, version, provider, arch string) error {
	m, ok := s.readMeta(name)
	i := findVersion(&m, version)
	if !ok || i < 0 {
		return registry.ErrPackageNotFound
	}
	v := &m.Versions[i]
	if provider == "" {
		for _, p := range v.Providers {
			_ = s.Delete(s.boxRel(name, version, p.Name, p.archKey()))
		}
		if _, _, errs := s.DeleteTree(path.Join(boxesDir, name, version)); len(errs) > 0 {
			return errs[0]
		}
		m.Versions = append(m.Versions[:i], m.Versions[i+1:]...)
	} else {
		j := findProvider(v, provider, arch)
		if j < 0 {
			return registry.ErrPackageNotFound
		}
		if err := s.Delete(s.boxRel(name, version, provider, arch)); err != nil {
			return err
		}
		v.Providers = append(v.Providers[:j], v.Providers[j+1:]...)
		if len(v.Providers) == 0 {
			m.Versions = append(m.Versions[:i], m.Versions[i+1:]...)
		}
	}
	return s.writeMeta(m)
}

func splitName(name string) (string, string, bool) {
	org, box, ok := strings.Cut(strings.Trim(name, "/"), "/")
	return org, box, ok && validSegment(org) && validSegment(box)
}

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	org, box, ok := splitName(name)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	name = org + "/" + box
	m, ok := s.readMeta(name)
	if !ok || len(m.Versions) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	sortVersions(m.Versions)
	d := &registry.GenericPackageDetail{LatestVersion: m.Versions[0].Version, Description: m.Description}
	for _, v := range m.Versions {
		row := registry.GenericVersionDetail{Version: v.Version, Metadata: map[string]string{}}
		if t, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
			row.PublishedAt = t.UTC().Format(time.RFC3339)
		}
		var provs []string
		for _, p := range v.Providers {
			f := registry.GenericFile{Name: p.Name + "/" + p.archKey() + ".box", Size: p.Size}
			if p.ChecksumType == "sha256" {
				f.SHA256 = p.Checksum
			}
			if fi, err := s.Stat(s.boxRel(name, v.Version, p.Name, p.archKey())); err == nil {
				f.Size = fi.Size
			}
			row.Size += f.Size
			row.Files = append(row.Files, f)
			label := p.Name
			if p.Architecture != "" {
				label += "/" + p.Architecture
			}
			provs = append(provs, label)
		}
		row.Metadata["providers"] = strings.Join(provs, ", ")
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	m, ok := s.readMeta(ref.Name)
	i := findVersion(&m, ref.Version)
	if !ok || i < 0 {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	t, _ := time.Parse(time.RFC3339, m.Versions[i].CreatedAt)
	return registry.ArtifactMeta{PublishedAt: t}, nil
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := parse(r.URL.Path)
	switch p.kind {
	case reqDownload, reqProvider:
		return registry.ArtifactRef{Name: p.name(), Version: p.version}, true
	case reqMeta, reqVersion, reqAPIBox, reqAPIVersion:
		return registry.ArtifactRef{Name: p.name()}, true
	}
	return registry.ArtifactRef{}, false
}

// ── Shared read handlers ──

// rewrite points every provider URL at this registry.
func rewrite(m Metadata, base string) Metadata {
	out := m
	out.Versions = make([]Version, len(m.Versions))
	for i, v := range m.Versions {
		nv := v
		nv.Providers = make([]Provider, len(v.Providers))
		for j, p := range v.Providers {
			p.URL = boxURL(base, m.Name, v.Version, p.Name, p.archKey())
			nv.Providers[j] = p
		}
		out.Versions[i] = nv
	}
	return out
}

func apiBox(m Metadata) map[string]any {
	org, box, _ := strings.Cut(m.Name, "/")
	out := map[string]any{
		"tag": m.Name, "username": org, "name": box,
		"short_description": m.Description, "description_markdown": m.Description,
		"versions": apiVersions(m.Versions),
	}
	if len(m.Versions) > 0 {
		out["current_version"] = apiVersion(m.Versions[0])
	}
	return out
}

func apiVersion(v Version) map[string]any {
	status := v.Status
	if status == "" {
		status = "active"
	}
	provs := make([]map[string]any, 0, len(v.Providers))
	for _, p := range v.Providers {
		provs = append(provs, map[string]any{
			"name": p.Name, "hosted": true, "download_url": p.URL, "original_url": p.URL,
			"checksum_type": p.ChecksumType, "checksum": p.Checksum,
			"architecture": p.Architecture, "default_architecture": p.DefaultArchitecture,
			"created_at": p.CreatedAt, "size": p.Size,
		})
	}
	return map[string]any{
		"version": v.Version, "status": status, "description_markdown": v.Description,
		"created_at": v.CreatedAt, "providers": provs,
	}
}

func apiVersions(vs []Version) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, apiVersion(v))
	}
	return out
}

// serveMeta writes the metadata (or an API view of it).
func serveMeta(w http.ResponseWriter, r *http.Request, m Metadata, p request) {
	m = rewrite(m, pkgbase.PublicBase(r))
	sortVersions(m.Versions)
	switch p.kind {
	case reqAPIBox:
		pkgbase.WriteJSON(w, r, http.StatusOK, apiBox(m))
	case reqAPIVersion, reqVersion:
		i := findVersion(&m, p.version)
		if i < 0 {
			pkgbase.NotFound(w)
			return
		}
		if p.kind == reqVersion {
			pkgbase.WriteJSON(w, r, http.StatusOK, m.Versions[i])
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, apiVersion(m.Versions[i]))
	default:
		pkgbase.WriteJSON(w, r, http.StatusOK, m)
	}
}

func serveSearch(w http.ResponseWriter, r *http.Request, s *Store) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	base := pkgbase.PublicBase(r)
	boxes := []map[string]any{}
	for _, n := range s.names() {
		if q != "" && !strings.Contains(strings.ToLower(n), q) {
			continue
		}
		m, ok := s.readMeta(n)
		if !ok || len(m.Versions) == 0 {
			continue
		}
		sortVersions(m.Versions)
		boxes = append(boxes, apiBox(rewrite(m, base)))
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"boxes": boxes})
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
	return detail(l.store, name)
}

func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	org, box, ok := splitName(name)
	if !ok {
		return registry.ErrPackageNotFound
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.store.deleteVersion(org+"/"+box, version, "", ""); err != nil {
		return err
	}
	l.EmitDeleted(org + "/" + box + "@" + version)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref)
}

// PromoteVersion copies every provider of name@version into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("vagrant: promote target %s/%s is not a vagrant local repository", dst.Namespace(), dst.Name())
	}
	org, box, ok := splitName(name)
	if !ok {
		return registry.ErrPackageNotFound
	}
	name = org + "/" + box
	m, ok := l.store.readMeta(name)
	i := findVersion(&m, version)
	if !ok || i < 0 {
		return registry.ErrPackageNotFound
	}
	v := m.Versions[i]
	for _, p := range v.Providers {
		rel := l.store.boxRel(name, version, p.Name, p.archKey())
		rc, fi, err := l.store.Open(rel)
		if err != nil {
			return err
		}
		err = d.store.WriteStream(rel, rc, fi.Size)
		rc.Close()
		if err != nil {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dm, _ := d.store.readMeta(name)
	if dm.Description == "" {
		dm.Description = m.Description
	}
	if j := findVersion(&dm, version); j >= 0 {
		dm.Versions[j] = v
	} else {
		dm.Versions = append(dm.Versions, v)
	}
	return d.store.writeMeta(dm)
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := parse(r.URL.Path)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		l.read(w, r, p)
	case http.MethodPut, http.MethodPost:
		if p.kind != reqProvider && p.kind != reqDownload {
			pkgbase.Error(w, http.StatusBadRequest, "expected PUT /{org}/{box}/{version}/{provider}[/{arch}]")
			return
		}
		l.upload(w, r, p)
	case http.MethodDelete:
		l.remove(w, p)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) read(w http.ResponseWriter, r *http.Request, p request) {
	switch p.kind {
	case reqSearch:
		serveSearch(w, r, l.store)
	case reqMeta, reqVersion, reqAPIBox, reqAPIVersion:
		m, ok := l.store.readMeta(p.name())
		if !ok || len(m.Versions) == 0 {
			pkgbase.NotFound(w)
			return
		}
		serveMeta(w, r, m, p)
	case reqDownload, reqProvider:
		if !pkgbase.ServeStored(w, r, l.store.Store, l.store.boxRel(p.name(), p.version, p.provider, p.arch), ctBox) {
			pkgbase.NotFound(w)
		}
	default:
		pkgbase.NotFound(w)
	}
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(b []byte) (int, error) {
	c.n += int64(len(b))
	return len(b), nil
}

// receiveBox streams body into rel and returns its size and sha256.
func receiveBox(st *pkgbase.Store, rel string, body io.Reader, size int64) (int64, string, error) {
	h := sha256.New()
	var cw countingWriter
	tee := io.TeeReader(body, io.MultiWriter(h, &cw))
	if size < 0 {
		b, err := io.ReadAll(tee)
		if err != nil {
			return 0, "", err
		}
		if err := st.Write(rel, b); err != nil {
			return 0, "", err
		}
	} else {
		err := st.WriteStream(rel, io.LimitReader(tee, size), size)
		if err == nil && cw.n != size {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			_ = st.Delete(rel)
			return 0, "", err
		}
	}
	return cw.n, hexSum(h), nil
}

func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func (l *Local) upload(w http.ResponseWriter, r *http.Request, p request) {
	if !l.CheckPush(w) {
		return
	}
	max := l.MaxUpload
	if max <= 0 {
		max = pkgbase.DefaultMaxUpload
	}
	if r.ContentLength > max {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %d bytes", max))
		return
	}
	name := p.name()
	m, _ := l.store.readMeta(name)
	exists := findVersion(&m, p.version) >= 0
	incoming := r.ContentLength
	if incoming < 0 {
		incoming = 0
	}
	if !l.AllowPublish(w, exists, incoming) {
		return
	}
	var body io.Reader = r.Body
	if r.ContentLength < 0 {
		body = io.LimitReader(r.Body, max+1)
	}
	rel := l.store.boxRel(name, p.version, p.provider, p.arch)
	size, sum, err := receiveBox(l.store.Store, rel, body, r.ContentLength)
	if err == nil && size > max {
		_ = l.store.Delete(rel)
		err = fmt.Errorf("upload exceeds %d bytes", max)
	}
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}
	if want := r.Header.Get("X-Checksum-Sha256"); want != "" && !strings.EqualFold(want, sum) {
		_ = l.store.Delete(rel)
		pkgbase.Error(w, http.StatusBadRequest, "sha256 mismatch")
		return
	}

	l.mu.Lock()
	m, _ = l.store.readMeta(name)
	m.Name = name
	if d := r.URL.Query().Get("description"); d != "" {
		m.Description = d
	}
	now := time.Now().UTC().Format(time.RFC3339)
	i := findVersion(&m, p.version)
	if i < 0 {
		m.Versions = append(m.Versions, Version{Version: p.version, Status: "active", CreatedAt: now})
		i = len(m.Versions) - 1
	}
	v := &m.Versions[i]
	prov := Provider{Name: p.provider, ChecksumType: "sha256", Checksum: sum, Size: size, CreatedAt: now}
	if p.arch != defaultArch {
		prov.Architecture = p.arch
	}
	if j := findProvider(v, p.provider, p.arch); j >= 0 {
		prov.DefaultArchitecture = v.Providers[j].DefaultArchitecture
		v.Providers[j] = prov
	} else {
		prov.DefaultArchitecture = true
		for _, o := range v.Providers {
			if o.Name == p.provider && o.DefaultArchitecture {
				prov.DefaultArchitecture = false
			}
		}
		v.Providers = append(v.Providers, prov)
	}
	if da := r.URL.Query().Get("default_architecture"); da != "" {
		set := da == "true" || da == "1"
		for j := range v.Providers {
			if v.Providers[j].Name == p.provider {
				if set {
					v.Providers[j].DefaultArchitecture = v.Providers[j].archKey() == p.arch
				} else if v.Providers[j].archKey() == p.arch {
					v.Providers[j].DefaultArchitecture = false
				}
			}
		}
	}
	err = l.store.writeMeta(m)
	l.mu.Unlock()
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(name+"@"+p.version+"/"+p.provider+"/"+p.arch, size)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"name": name, "version": p.version, "provider": p.provider, "architecture": prov.Architecture,
		"size": size, "checksum_type": "sha256", "checksum": sum,
		"url": boxURL(pkgbase.PublicBase(r), name, p.version, p.provider, p.arch),
	})
}

func (l *Local) remove(w http.ResponseWriter, p request) {
	if !l.CheckPush(w) {
		return
	}
	var err error
	l.mu.Lock()
	switch p.kind {
	case reqVersion:
		err = l.store.deleteVersion(p.name(), p.version, "", "")
	case reqProvider, reqDownload:
		err = l.store.deleteVersion(p.name(), p.version, p.provider, p.arch)
	default:
		err = registry.ErrPackageNotFound
	}
	l.mu.Unlock()
	if err != nil {
		if pkgbase.IsNotFound(err) {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	subject := p.name() + "@" + p.version
	if p.provider != "" {
		subject += "/" + p.provider + "/" + p.arch
	}
	l.EmitDeleted(subject)
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store    *Store
	inflight sync.Map
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		if r.URL == "" {
			cp := *r
			cp.URL = DefaultUpstream
			r = &cp
		}
		b, err := upstream.BuildRemote(deps, typ+"/remote", ns, r, 5*time.Minute, upstream.RemoteBuildOptions{ClientTimeout: 2 * time.Hour})
		if err != nil {
			return nil, err
		}
		base := &pkgbase.RemoteRepo{
			Repo: pkgbase.Repo{
				NS: ns, RepoName: r.Name, RepoType: typ, RepoKind: service.RegistryKindRemote,
				MaxUpload: r.MaxUploadSize, Emitter: deps.Emitter, Store: pkgbase.NewStore(b.FS, b.BasePath),
			},
			Client:     b.Client,
			Router:     upstream.NewRouter(b.Client, b.Upstreams),
			MutableTTL: b.MutableTTL,
			ProbePath:  "/",
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

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (rr *Remote) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(rr.store, ref)
}

// PurgeCache drops cached metadata (upstream copies and the local
// projection); opts.All also removes cached boxes.
func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	if opts.All {
		return rr.Purge(opts), nil
	}
	out := rr.Purge(opts, metaDir)
	for _, n := range rr.store.names() {
		rel := rr.store.metaRel(n)
		if fi, err := rr.store.Stat(rel); err == nil {
			if err := rr.store.Delete(rel); err != nil {
				out.Errors = append(out.Errors, err.Error())
				continue
			}
			out.PurgedFiles++
			out.PurgedBytes += fi.Size
		}
	}
	return out, nil
}

func (rr *Remote) upstreamMetaRel(name string) string { return path.Join(metaDir, name+".json") }

// metadata returns the upstream metadata document of name (cached
// with MutableTTL, stale-on-error).
func (rr *Remote) metadata(ctx context.Context, name string) (Metadata, error) {
	rel := rr.upstreamMetaRel(name)
	var body []byte
	if rr.store.Exists(rel) && rr.Fresh(rel) {
		body, _ = rr.store.Read(rel)
	}
	if body == nil {
		upath := "/" + name
		resp, err := rr.Router.For(upath).GetWithHeaders(ctx, upath, http.Header{"Accept": {"application/json"}})
		if err == nil {
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if err != nil {
			stale, rerr := rr.store.Read(rel)
			if rerr != nil || pkgbase.IsNotFound(err) {
				return Metadata{}, err
			}
			body = stale
		} else {
			_ = rr.store.Write(rel, body)
		}
	}
	var m Metadata
	if err := json.Unmarshal(body, &m); err != nil {
		return Metadata{}, fmt.Errorf("vagrant: upstream metadata for %s: %w", name, err)
	}
	if m.Name == "" {
		m.Name = name
	}
	return m, nil
}

// project records the upstream metadata in the local layout so
// listing / detail work against the cache.
func (rr *Remote) project(m Metadata) {
	cur, ok := rr.store.readMeta(m.Name)
	if ok {
		if b1, _ := pkgbase.MarshalJSON(cur); b1 != nil {
			if b2, _ := pkgbase.MarshalJSON(m); string(b1) == string(b2) {
				return
			}
		}
	}
	_ = rr.store.writeMeta(m)
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := parse(r.URL.Path)
	switch p.kind {
	case reqMeta, reqVersion, reqAPIBox, reqAPIVersion:
		m, err := rr.metadata(r.Context(), p.name())
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		m.Name = p.name()
		rr.project(m)
		serveMeta(w, r, m, p)
	case reqDownload, reqProvider:
		rr.serveBox(w, r, p)
	case reqSearch:
		serveSearch(w, r, rr.store)
	default:
		pkgbase.NotFound(w)
	}
}

func (rr *Remote) providerURL(ctx context.Context, p request) (string, error) {
	m, err := rr.metadata(ctx, p.name())
	if err != nil {
		return "", err
	}
	i := findVersion(&m, p.version)
	if i < 0 {
		return "", fmt.Errorf("vagrant: %s@%s: %w", p.name(), p.version, upstream.ErrNotFound)
	}
	j := findProvider(&m.Versions[i], p.provider, p.arch)
	if j < 0 && p.arch == defaultArch {
		for k, pr := range m.Versions[i].Providers {
			if pr.Name == p.provider && (j < 0 || pr.DefaultArchitecture) {
				j = k
			}
		}
	}
	if j < 0 || m.Versions[i].Providers[j].URL == "" {
		return "", fmt.Errorf("vagrant: %s@%s/%s: %w", p.name(), p.version, p.provider, upstream.ErrNotFound)
	}
	return m.Versions[i].Providers[j].URL, nil
}

type lenientWriter struct {
	w      io.Writer
	failed bool
}

func (l *lenientWriter) Write(b []byte) (int, error) {
	if !l.failed {
		if _, err := l.w.Write(b); err != nil {
			l.failed = true
		}
	}
	return len(b), nil
}

// fill downloads the box for p into the cache. start (optional) is
// invoked once upstream answered and returns a writer for a live copy.
func (rr *Remote) fill(ctx context.Context, p request, start func(*upstream.Response) io.Writer) error {
	u, err := rr.providerURL(ctx, p)
	if err != nil {
		return err
	}
	resp, err := rr.Client.Get(ctx, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var sink io.Writer = io.Discard
	if start != nil {
		if s := start(resp); s != nil {
			sink = &lenientWriter{w: s}
		}
	}
	rel := rr.store.boxRel(p.name(), p.version, p.provider, p.arch)
	if _, busy := rr.inflight.LoadOrStore(rel, struct{}{}); busy {
		_, err := io.Copy(sink, resp.Body)
		return err
	}
	defer rr.inflight.Delete(rel)
	_, _, err = receiveBox(rr.store.Store, rel, io.TeeReader(resp.Body, sink), resp.ContentLength)
	return err
}

func (rr *Remote) serveBox(w http.ResponseWriter, r *http.Request, p request) {
	rel := rr.store.boxRel(p.name(), p.version, p.provider, p.arch)
	if pkgbase.ServeStored(w, r, rr.store.Store, rel, ctBox) {
		return
	}
	started := false
	err := rr.fill(context.WithoutCancel(r.Context()), p, func(resp *upstream.Response) io.Writer {
		started = true
		w.Header().Set("Content-Type", ctBox)
		if resp.ContentLength >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return nil
		}
		return w
	})
	if err != nil && !started {
		pkgbase.UpstreamError(w, err)
	}
}

// Prefetch warms the metadata of name and every provider box of
// version (or the newest version).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	org, box, ok := splitName(name)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	name = org + "/" + box
	m, err := rr.metadata(ctx, name)
	if err != nil {
		return err
	}
	m.Name = name
	rr.project(m)
	sortVersions(m.Versions)
	if version == "" {
		if len(m.Versions) == 0 {
			return registry.ErrPackageNotFound
		}
		version = m.Versions[0].Version
	}
	i := findVersion(&m, version)
	if i < 0 {
		return registry.ErrPackageNotFound
	}
	for _, pr := range m.Versions[i].Providers {
		p := request{org: org, box: box, version: version, provider: pr.Name, arch: pr.archKey()}
		if !validSegment(p.provider) || !validSegment(p.arch) {
			continue
		}
		if rr.store.Exists(rr.store.boxRel(name, version, p.provider, p.arch)) {
			continue
		}
		if err := rr.fill(ctx, p, nil); err != nil {
			return err
		}
	}
	return nil
}

// ── Virtual ──

// Virtual merges box metadata (versions/providers) across members and
// serves downloads first-hit.
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
	p := parse(r.URL.Path)
	switch p.kind {
	case reqMeta, reqVersion, reqAPIBox, reqAPIVersion:
		mr := r.Clone(r.Context())
		mr.URL.Path = "/" + p.name()
		var docs []Metadata
		for _, b := range v.CollectMembers(mr) {
			var m Metadata
			if json.Unmarshal(b, &m) == nil {
				docs = append(docs, m)
			}
		}
		if len(docs) == 0 {
			pkgbase.NotFound(w)
			return
		}
		m := mergeMetadata(docs)
		m.Name = p.name()
		serveMeta(w, r, m, p)
	case reqSearch:
		seen := map[string]bool{}
		boxes := []json.RawMessage{}
		for _, b := range v.CollectMembers(r) {
			var res struct {
				Boxes []json.RawMessage `json:"boxes"`
			}
			if json.Unmarshal(b, &res) != nil {
				continue
			}
			for _, bx := range res.Boxes {
				var tag struct {
					Tag string `json:"tag"`
				}
				_ = json.Unmarshal(bx, &tag)
				if !seen[tag.Tag] {
					seen[tag.Tag] = true
					boxes = append(boxes, bx)
				}
			}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"boxes": boxes})
	default:
		if !v.ServeFirstHit(w, r) {
			pkgbase.NotFound(w)
		}
	}
}
