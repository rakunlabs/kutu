package cargo

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// publishDep is one dependency in the `cargo publish` metadata.
type publishDep struct {
	Name               string   `json:"name"`
	VersionReq         string   `json:"version_req"`
	Features           []string `json:"features"`
	Optional           bool     `json:"optional"`
	DefaultFeatures    bool     `json:"default_features"`
	Target             *string  `json:"target"`
	Kind               string   `json:"kind"`
	Registry           *string  `json:"registry"`
	ExplicitNameInToml *string  `json:"explicit_name_in_toml"`
}

// publishMeta is the JSON metadata half of a `crates/new` body.
type publishMeta struct {
	Name          string              `json:"name"`
	Vers          string              `json:"vers"`
	Deps          []publishDep        `json:"deps"`
	Features      map[string][]string `json:"features"`
	Authors       []string            `json:"authors"`
	Description   *string             `json:"description"`
	Documentation *string             `json:"documentation"`
	Homepage      *string             `json:"homepage"`
	Keywords      []string            `json:"keywords"`
	Categories    []string            `json:"categories"`
	License       *string             `json:"license"`
	LicenseFile   *string             `json:"license_file"`
	Repository    *string             `json:"repository"`
	Links         *string             `json:"links"`
	RustVersion   *string             `json:"rust_version"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// parsePublishBody splits the cargo publish wire format: u32 LE json
// length, json, u32 LE crate length, crate bytes.
func parsePublishBody(body []byte) (*publishMeta, []byte, error) {
	if len(body) < 4 {
		return nil, nil, fmt.Errorf("publish body too short")
	}
	jl := int(binary.LittleEndian.Uint32(body[:4]))
	if jl < 0 || 4+jl+4 > len(body) {
		return nil, nil, fmt.Errorf("invalid metadata length")
	}
	var meta publishMeta
	if err := json.Unmarshal(body[4:4+jl], &meta); err != nil {
		return nil, nil, fmt.Errorf("invalid metadata json: %w", err)
	}
	off := 4 + jl
	cl := int(binary.LittleEndian.Uint32(body[off : off+4]))
	off += 4
	if cl < 0 || off+cl != len(body) {
		return nil, nil, fmt.Errorf("invalid crate length")
	}
	return &meta, body[off : off+cl], nil
}

func isV2Feature(vals []string) bool {
	for _, v := range vals {
		if strings.HasPrefix(v, "dep:") || strings.Contains(v, "?/") {
			return true
		}
	}
	return false
}

// buildEntry converts publish metadata into a sparse index entry.
func buildEntry(m *publishMeta, crate []byte) indexEntry {
	ent := indexEntry{
		Name: m.Name, Version: m.Vers, CKSum: pkgbase.SHA256Hex(crate),
		Deps: make([]indexDep, 0, len(m.Deps)), Features: map[string][]string{},
		Links: m.Links, RustVersion: str(m.RustVersion),
		PubTime: time.Now().UTC().Format(time.RFC3339),
	}
	for _, d := range m.Deps {
		kind := d.Kind
		if kind == "" {
			kind = "normal"
		}
		feats := d.Features
		if feats == nil {
			feats = []string{}
		}
		id := indexDep{
			Name: d.Name, Req: d.VersionReq, Features: feats, Optional: d.Optional,
			DefaultFeatures: d.DefaultFeatures, Target: d.Target, Kind: kind, Registry: d.Registry,
		}
		if d.ExplicitNameInToml != nil && *d.ExplicitNameInToml != "" && *d.ExplicitNameInToml != d.Name {
			pkg := d.Name
			id.Name = *d.ExplicitNameInToml
			id.Package = &pkg
		}
		ent.Deps = append(ent.Deps, id)
	}
	for k, v := range m.Features {
		if v == nil {
			v = []string{}
		}
		if isV2Feature(v) {
			if ent.Features2 == nil {
				ent.Features2 = map[string][]string{}
			}
			ent.Features2[k] = v
			continue
		}
		ent.Features[k] = v
	}
	if ent.Features2 != nil {
		ent.V = 2
	}
	return ent
}

func metaFromPublish(m *publishMeta) *versionMeta {
	return &versionMeta{
		Name: m.Name, Version: m.Vers, Description: str(m.Description), License: str(m.License),
		LicenseFile: str(m.LicenseFile), Homepage: str(m.Homepage), Documentation: str(m.Documentation),
		Repository: str(m.Repository), Keywords: m.Keywords, Categories: m.Categories, Authors: m.Authors,
		PublishedAt: time.Now().UTC(),
	}
}

// ── Local ──

// Local is a hosted cargo registry.
type Local struct {
	*pkgbase.Repo
	store *Store
}

// NewLocalFactory builds local cargo repositories.
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
	return l.store.listPackages()
}

func (l *Local) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, l, l.store.Store), nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return packageDetail(l.store, name)
}

func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	if err := l.store.DeleteVersion(name, version); err != nil {
		return err
	}
	l.EmitDeleted(norm(name) + "@" + version)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref)
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	ents, _ := s.ReadIndexByName(ref.Name)
	ent, ok := findEntry(ents, ref.Version)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	var out registry.ArtifactMeta
	if vm := s.readMeta(ref.Name, ref.Version); vm != nil {
		out.License = vm.License
		out.PublishedAt = vm.PublishedAt
	}
	if out.PublishedAt.IsZero() && ent.PubTime != "" {
		out.PublishedAt, _ = time.Parse(time.RFC3339, ent.PubTime)
	}
	if out.PublishedAt.IsZero() {
		if fi, err := s.Stat(crateRel(ref.Name, ref.Version)); err == nil {
			out.PublishedAt = fi.ModTime
		}
	}
	return out, nil
}

// PromoteVersion copies name@version (crate, index line, metadata) into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("cargo: promote target %s/%s is not a local cargo repository", dst.Namespace(), dst.Name())
	}
	ents, err := l.store.ReadIndexByName(name)
	if err != nil {
		return err
	}
	ent, ok := findEntry(ents, version)
	if !ok {
		return registry.ErrPackageNotFound
	}
	crate, err := l.store.Read(crateRel(name, version))
	if err != nil {
		return err
	}
	if _, gerr := d.Guard.Check(d.store.Store, d.store.hasVersion(name, version), int64(len(crate))); gerr != nil {
		return gerr
	}
	if err := d.store.put(ent, l.store.readMeta(name, version), crate); err != nil {
		return err
	}
	if d.store.readOwners(name) == nil {
		if o := l.store.readOwners(name); o != nil {
			_ = d.store.writeOwners(ent.Name, o)
		}
	}
	d.EmitPublished(norm(name)+"@"+version, int64(len(crate)))
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/config.json" && pkgbase.IsRead(r):
		serveConfig(w, r)
	case p == "/api/v1/crates" && pkgbase.IsRead(r):
		pkgbase.WriteJSON(w, r, http.StatusOK, l.store.search(r.URL.Query().Get("q"), perPage(r)))
	case p == "/api/v1/crates/new" && r.Method == http.MethodPut:
		l.publishNew(w, r)
	case strings.HasPrefix(p, "/api/v1/crates/"):
		l.serveCrateAPI(w, r)
	case pkgbase.IsRead(r):
		idx := strings.ToLower(strings.Trim(p, "/"))
		if _, ok := parseIndexPath(idx); !ok {
			pkgbase.NotFound(w)
			return
		}
		serveStoredIndex(w, r, l.store, idx)
	default:
		cargoError(w, http.StatusNotFound, "no cargo route")
	}
}

func (l *Local) serveCrateAPI(w http.ResponseWriter, r *http.Request) {
	name, rest, ok := apiCrateRoute(r.URL.Path)
	if !ok {
		cargoError(w, http.StatusNotFound, "no cargo route")
		return
	}
	switch {
	case len(rest) == 2 && rest[1] == "download":
		switch {
		case pkgbase.IsRead(r):
			if !validVersion(rest[0]) || !pkgbase.ServeStored(w, r, l.store.Store, crateRel(name, rest[0]), "application/x-tar") {
				cargoError(w, http.StatusNotFound, "crate not found")
			}
		case r.Method == http.MethodPut:
			l.publishRaw(w, r, name, rest[0])
		default:
			cargoError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case len(rest) == 2 && rest[1] == "yank" && r.Method == http.MethodDelete:
		l.yank(w, r, name, rest[0], true)
	case len(rest) == 2 && rest[1] == "unyank" && r.Method == http.MethodPut:
		l.yank(w, r, name, rest[0], false)
	case len(rest) == 1 && rest[0] == "owners":
		l.owners(w, r, name)
	default:
		cargoError(w, http.StatusNotFound, "no cargo route")
	}
}

func (l *Local) checkPush(w http.ResponseWriter) bool {
	if !l.AllowPush {
		cargoError(w, http.StatusMethodNotAllowed, "push disabled for this repository")
		return false
	}
	return true
}

func (l *Local) guard(w http.ResponseWriter, name, version string, size int) bool {
	if code, err := l.Guard.Check(l.store.Store, l.store.hasVersion(name, version), int64(size)); err != nil {
		cargoError(w, code, err.Error())
		return false
	}
	return true
}

func (l *Local) publishNew(w http.ResponseWriter, r *http.Request) {
	if !l.checkPush(w) {
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		cargoError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	meta, crate, err := parsePublishBody(body)
	if err != nil {
		cargoError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validName(meta.Name) {
		cargoError(w, http.StatusBadRequest, "invalid crate name "+meta.Name)
		return
	}
	if !validVersion(meta.Vers) {
		cargoError(w, http.StatusBadRequest, "invalid version "+meta.Vers)
		return
	}
	ents, _ := l.store.ReadIndexByName(meta.Name)
	if len(ents) > 0 && ents[0].Name != meta.Name {
		cargoError(w, http.StatusConflict, fmt.Sprintf("crate name conflicts with existing crate %q", ents[0].Name))
		return
	}
	if _, exists := findEntry(ents, meta.Vers); exists {
		cargoError(w, http.StatusConflict, fmt.Sprintf("crate version `%s@%s` is already uploaded", meta.Name, meta.Vers))
		return
	}
	if !l.guard(w, meta.Name, meta.Vers, len(crate)) {
		return
	}
	if err := l.store.put(buildEntry(meta, crate), metaFromPublish(meta), crate); err != nil {
		cargoError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(ents) == 0 && l.store.readOwners(meta.Name) == nil {
		if u := userLogin(r); u != "" {
			_ = l.store.writeOwners(meta.Name, []owner{{Login: u}})
		}
	}
	l.EmitPublished(norm(meta.Name)+"@"+meta.Vers, int64(len(crate)))
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
		"warnings": map[string][]string{"invalid_categories": {}, "invalid_badges": {}, "other": {}},
	})
}

func (l *Local) publishRaw(w http.ResponseWriter, r *http.Request, name, version string) {
	if !l.checkPush(w) {
		return
	}
	if !validVersion(version) {
		cargoError(w, http.StatusBadRequest, "invalid version "+version)
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		cargoError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if !l.guard(w, name, version, len(body)) {
		return
	}
	if err := l.store.WriteCrate(name, version, body); err != nil {
		cargoError(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(norm(name)+"@"+version, int64(len(body)))
	w.WriteHeader(http.StatusCreated)
}

func (l *Local) yank(w http.ResponseWriter, r *http.Request, name, version string, yanked bool) {
	if !l.checkPush(w) {
		return
	}
	if err := l.store.setYanked(name, version, yanked); err != nil {
		if pkgbase.IsNotFound(err) {
			cargoError(w, http.StatusNotFound, fmt.Sprintf("crate `%s@%s` not found", name, version))
			return
		}
		cargoError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]bool{"ok": true})
}

func (l *Local) owners(w http.ResponseWriter, r *http.Request, name string) {
	if ents, _ := l.store.ReadIndexByName(name); len(ents) == 0 {
		cargoError(w, http.StatusNotFound, fmt.Sprintf("crate `%s` not found", name))
		return
	}
	current := l.store.readOwners(name)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if current == nil {
			current = []owner{}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"users": current})
		return
	case http.MethodPut, http.MethodDelete:
	default:
		cargoError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !l.checkPush(w) {
		return
	}
	var req struct {
		Users []string `json:"users"`
	}
	body, err := pkgbase.ReadBody(r.Body, 1<<20)
	if err != nil || json.Unmarshal(body, &req) != nil || len(req.Users) == 0 {
		cargoError(w, http.StatusBadRequest, `expected {"users":[...]}`)
		return
	}
	var msg string
	if r.Method == http.MethodPut {
		for _, u := range req.Users {
			if !hasOwner(current, u) {
				current = append(current, owner{Login: u})
			}
		}
		msg = fmt.Sprintf("user(s) %s has been added as owner(s) of crate %s", strings.Join(req.Users, ", "), name)
	} else {
		next := current[:0]
		for _, o := range current {
			if !contains(req.Users, o.Login) {
				next = append(next, o)
			}
		}
		current = next
		msg = fmt.Sprintf("user(s) %s has been removed as owner(s) of crate %s", strings.Join(req.Users, ", "), name)
	}
	if err := l.store.writeOwners(name, current); err != nil {
		cargoError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"ok": true, "msg": msg})
}

func hasOwner(os []owner, login string) bool {
	for _, o := range os {
		if o.Login == login {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// userLogin best-effort extracts a Basic-auth username for the
// initial owner list; token-only callers get no implicit owner.
func userLogin(r *http.Request) string {
	if u, _, ok := r.BasicAuth(); ok && u != "" && !strings.HasPrefix(u, "kutu_") {
		return u
	}
	return ""
}
