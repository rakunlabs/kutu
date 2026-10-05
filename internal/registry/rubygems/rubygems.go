// Package rubygems implements a RubyGems registry usable by `gem` and
// `bundler`:
//
//	GET    /versions                                  compact index: all gems
//	GET    /info/{gem}                                compact index: one gem
//	GET    /names                                     compact index: gem names
//	GET    /gems/{name}-{version}[-{platform}].gem    download
//	GET    /quick/Marshal.4.8/{full}.gemspec.rz       zlib'd Marshal gemspec
//	GET    /specs.4.8.gz                              legacy full index
//	GET    /latest_specs.4.8.gz                       legacy latest index
//	GET    /prerelease_specs.4.8.gz                   legacy prerelease index
//	POST   /api/v1/gems                               push (raw .gem body)
//	DELETE /api/v1/gems/yank                          yank (gem_name, version, platform)
//	GET    /api/v1/gems/{name}.json                   latest version info
//	GET    /api/v1/versions/{name}.json               version list
//	GET    /api/v1/dependencies[.json]?gems=a,b       dependency API (Marshal / JSON)
//	GET    /api/v1/api_key                            echoes the presented token (gem signin)
//
// Local repos generate the compact index on push/yank and synthesise
// the Marshal documents (legacy indexes, quick gemspecs, dependency
// API) from the stored metadata. Remote repos proxy an upstream such
// as https://rubygems.org: compact index and legacy indexes are cached
// with MutableTTL (streamed to storage), gem files and quick gemspecs
// forever. Virtual repos merge /versions, /info/{gem} and /names
// across members; everything else is first-hit.
//
// Client configuration:
//
//	# ~/.gem/credentials (chmod 0600)
//	:kutu: <token>
//	gem push --host https://kutu.example.com/registries/{ns}/{repo} -k kutu pkg.gem
//
//	# Gemfile
//	source "https://kutu.example.com/registries/{ns}/{repo}"
//	bundle config set --global https://kutu.example.com/registries/{ns}/{repo}/ x:<token>
//
//	# gem install
//	gem sources --add https://x:<token>@kutu.example.com/registries/{ns}/{repo}/
package rubygems

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeRubyGems

const (
	gemsDir  = "gems"
	metaDir  = "meta"
	indexDir = "index"

	compactType = "text/plain; charset=utf-8"
	binaryType  = "application/octet-stream"
)

// Store wraps pkgbase.Store with the rubygems layout:
//
//	gems/{full}.gem             gem files
//	meta/{name}/{vp}.json       GemMeta per version-platform
//	index/info/{name}           generated compact index files
//	index/versions, index/names
type Store struct {
	*pkgbase.Store
	mu *sync.Mutex
}

func newStore(s *pkgbase.Store) *Store { return &Store{Store: s, mu: &sync.Mutex{}} }

func gemRel(full string) string           { return path.Join(gemsDir, full+".gem") }
func metaRel(name, vp string) string      { return path.Join(metaDir, name, vp+".json") }
func localInfoRel(name string) string     { return path.Join(indexDir, "info", name) }
func localVersionsRel() string            { return path.Join(indexDir, "versions") }
func localNamesRel() string               { return path.Join(indexDir, "names") }
func (s *Store) names() ([]string, error) { return s.ListDirs(metaDir) }

// LoadMetas returns every stored version-platform of name, ascending.
func (s *Store) LoadMetas(name string) ([]*GemMeta, error) {
	if !validName(name) {
		return nil, nil
	}
	files, err := s.ListFiles(path.Join(metaDir, name))
	if err != nil {
		return nil, err
	}
	var out []*GemMeta
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		b, err := s.Read(path.Join(metaDir, name, f.Name))
		if err != nil {
			continue
		}
		var m GemMeta
		if json.Unmarshal(b, &m) == nil && m.Name != "" {
			out = append(out, &m)
		}
	}
	sortMetas(out)
	return out, nil
}

func sortMetas(ms []*GemMeta) {
	sort.SliceStable(ms, func(i, j int) bool {
		if c := pkgbase.CompareVersions(ms[i].Version, ms[j].Version); c != 0 {
			return c < 0
		}
		return ms[i].platform() < ms[j].platform()
	})
}

func (s *Store) allMetas() []*GemMeta {
	names, _ := s.names()
	var out []*GemMeta
	for _, n := range names {
		ms, _ := s.LoadMetas(n)
		out = append(out, ms...)
	}
	return out
}

func (s *Store) loadMeta(name, vp string) (*GemMeta, error) {
	b, err := s.Read(metaRel(name, vp))
	if err != nil {
		return nil, err
	}
	var m GemMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// put stores a gem and its metadata (caller regenerates indexes).
func (s *Store) put(m *GemMeta, body []byte) error {
	if err := s.Write(gemRel(m.FullName()), body); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.Write(metaRel(m.Name, m.versionPlatform()), b)
}

func (s *Store) remove(m *GemMeta) error {
	if err := s.Delete(gemRel(m.FullName())); err != nil {
		return err
	}
	return s.Delete(metaRel(m.Name, m.versionPlatform()))
}

// matchVersion selects metas of name matching version (plain version
// = every platform, "version-platform" = one).
func (s *Store) matchVersion(name, version, platform string) ([]*GemMeta, error) {
	ms, err := s.LoadMetas(name)
	if err != nil {
		return nil, err
	}
	var out []*GemMeta
	for _, m := range ms {
		switch {
		case platform != "":
			if m.Version == version && m.platform() == platform {
				out = append(out, m)
			}
		case m.Version == version || m.versionPlatform() == version:
			out = append(out, m)
		}
	}
	return out, nil
}

// ── compact index generation ──

func reqString(reqs []string) string { return strings.Join(reqs, "&") }

func isDefaultReq(reqs []string) bool {
	return len(reqs) == 0 || (len(reqs) == 1 && strings.TrimSpace(reqs[0]) == ">= 0")
}

func infoLine(m *GemMeta) string {
	var b strings.Builder
	b.WriteString(m.versionPlatform())
	b.WriteByte(' ')
	for i, d := range m.runtimeDeps() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(d.Name + ":" + reqString(d.Requirements))
	}
	b.WriteString("|checksum:" + m.SHA256)
	if !isDefaultReq(m.RequiredRuby) {
		b.WriteString(",ruby:" + reqString(m.RequiredRuby))
	}
	if !isDefaultReq(m.RequiredRubygems) {
		b.WriteString(",rubygems:" + reqString(m.RequiredRubygems))
	}
	return b.String()
}

func buildInfo(ms []*GemMeta) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, m := range ms {
		b.WriteString(infoLine(m))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// infoVersions returns the version-platform tokens of an info file.
func infoVersions(info []byte) []string {
	var out []string
	for _, line := range infoLines(info) {
		if tok, _, _ := strings.Cut(line, " "); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// infoLines returns the non-header lines of an info file.
func infoLines(info []byte) []string {
	var out []string
	started := false
	for _, line := range strings.Split(string(info), "\n") {
		line = strings.TrimRight(line, "\r")
		if !started {
			if line == "---" {
				started = true
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	if !started {
		for _, line := range strings.Split(string(info), "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

func versionsHeader() string {
	return "created_at: " + time.Now().UTC().Format(time.RFC3339) + "\n---\n"
}

type versionsEntry struct {
	versions []string
	md5      string
}

func buildVersions(names []string, entries map[string]versionsEntry) []byte {
	var b strings.Builder
	b.WriteString(versionsHeader())
	for _, n := range names {
		e := entries[n]
		if len(e.versions) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s\n", n, strings.Join(e.versions, ","), e.md5)
	}
	return []byte(b.String())
}

func buildNames(names []string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Regenerate rebuilds /info/{name}, /versions and /names.
func (s *Store) Regenerate(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.LoadMetas(name)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		if err := s.Delete(localInfoRel(name)); err != nil {
			return err
		}
	} else if err := s.Write(localInfoRel(name), buildInfo(ms)); err != nil {
		return err
	}
	return s.rebuildRoot()
}

func (s *Store) rebuildRoot() error {
	names, err := s.names()
	if err != nil {
		return err
	}
	entries := map[string]versionsEntry{}
	var present []string
	for _, n := range names {
		info, err := s.Read(localInfoRel(n))
		if err != nil {
			continue
		}
		vs := infoVersions(info)
		if len(vs) == 0 {
			continue
		}
		entries[n] = versionsEntry{versions: vs, md5: md5Hex(info)}
		present = append(present, n)
	}
	if err := s.Write(localVersionsRel(), buildVersions(present, entries)); err != nil {
		return err
	}
	return s.Write(localNamesRel(), buildNames(present))
}

// ListPackages lists every gem with its version-platform tokens.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := s.names()
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, n := range names {
		ms, _ := s.LoadMetas(n)
		if len(ms) == 0 {
			continue
		}
		out = append(out, registry.PackageSummary{Name: n, Versions: uniqueVersions(ms)})
	}
	return out, nil
}

func uniqueVersions(ms []*GemMeta) []string {
	seen := map[string]bool{}
	var vs []string
	for _, m := range ms {
		if !seen[m.Version] {
			seen[m.Version] = true
			vs = append(vs, m.Version)
		}
	}
	pkgbase.SortVersions(vs)
	return vs
}

// ── HTTP helpers ──

func reprDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

func etagMatches(r *http.Request, etag string) bool {
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return false
	}
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// serveCompact writes a compact index document with ETag/Repr-Digest.
// Range requests are answered with the full body: generated files are
// rewritten rather than appended, so partial responses would corrupt
// a client's local copy.
func serveCompact(w http.ResponseWriter, r *http.Request, body []byte) {
	etag := `"` + md5Hex(body) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Repr-Digest", reprDigest(body))
	h.Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(sha256Sum(body)))
	h.Set("Cache-Control", "max-age=60, public")
	if etagMatches(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, compactType, body)
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// presentedToken extracts the credential the client authenticated with.
func presentedToken(r *http.Request) string {
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	a := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return a
}

// formValues merges query and url-encoded body (DELETE bodies are not
// parsed by net/http).
func formValues(r *http.Request, max int64) url.Values {
	vals := r.URL.Query()
	if r.Body == nil {
		return vals
	}
	body, err := pkgbase.ReadBody(r.Body, max)
	if err != nil || len(body) == 0 {
		return vals
	}
	if bv, err := url.ParseQuery(string(body)); err == nil {
		for k, v := range bv {
			vals[k] = append(vals[k], v...)
		}
	}
	return vals
}

func gemFileFromPath(p string) (full string, ok bool) {
	file, found := strings.CutPrefix(p, "/gems/")
	if !found || strings.Contains(file, "/") || !strings.HasSuffix(file, ".gem") {
		return "", false
	}
	full = strings.TrimSuffix(file, ".gem")
	return full, full != ""
}

func quickFromPath(p string) (full string, ok bool) {
	file, found := strings.CutPrefix(p, "/quick/Marshal.4.8/")
	if !found || strings.Contains(file, "/") || !strings.HasSuffix(file, ".gemspec.rz") {
		return "", false
	}
	full = strings.TrimSuffix(file, ".gemspec.rz")
	return full, full != ""
}

func singleSegment(p, prefix, suffix string) (string, bool) {
	s, found := strings.CutPrefix(p, prefix)
	if !found || !strings.HasSuffix(s, suffix) {
		return "", false
	}
	s = strings.TrimSuffix(s, suffix)
	if !validName(s) {
		return "", false
	}
	return s, true
}

func isSpecsIndex(p string) bool {
	switch strings.TrimPrefix(p, "/") {
	case "specs.4.8.gz", "latest_specs.4.8.gz", "prerelease_specs.4.8.gz",
		"specs.4.8", "latest_specs.4.8", "prerelease_specs.4.8":
		return true
	}
	return false
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := r.URL.Path
	if full, ok := gemFileFromPath(p); ok {
		if n, v, _, ok := splitGemFile(full); ok {
			return registry.ArtifactRef{Name: n, Version: v}, true
		}
		return registry.ArtifactRef{}, false
	}
	if full, ok := quickFromPath(p); ok {
		if n, _, _, ok := splitGemFile(full); ok {
			return registry.ArtifactRef{Name: n}, true
		}
		return registry.ArtifactRef{}, false
	}
	for _, pre := range [][2]string{{"/info/", ""}, {"/api/v1/gems/", ".json"}, {"/api/v1/versions/", ".json"}} {
		if n, ok := singleSegment(p, pre[0], pre[1]); ok {
			return registry.ArtifactRef{Name: n}, true
		}
	}
	return registry.ArtifactRef{}, false
}

// ── JSON API shapes ──

func depsJSON(ds []Dependency) []map[string]string {
	out := []map[string]string{}
	for _, d := range ds {
		out = append(out, map[string]string{"name": d.Name, "requirements": strings.Join(d.Requirements, ", ")})
	}
	return out
}

func gemJSON(base string, m *GemMeta) map[string]any {
	var dev, run []Dependency
	for _, d := range m.Dependencies {
		if d.Type == "development" {
			dev = append(dev, d)
		} else {
			run = append(run, d)
		}
	}
	return map[string]any{
		"name":               m.Name,
		"downloads":          0,
		"version":            m.Version,
		"version_created_at": m.CreatedAt.UTC().Format(time.RFC3339Nano),
		"version_downloads":  0,
		"platform":           m.platform(),
		"authors":            strings.Join(m.Authors, ", "),
		"info":               firstNonEmpty(m.Description, m.Summary),
		"licenses":           nonNil(m.Licenses),
		"metadata":           m.Metadata,
		"yanked":             false,
		"sha":                m.SHA256,
		"gem_uri":            base + "/gems/" + m.FullName() + ".gem",
		"homepage_uri":       m.Homepage,
		"dependencies":       map[string]any{"development": depsJSON(dev), "runtime": depsJSON(run)},
	}
}

func versionJSON(m *GemMeta) map[string]any {
	return map[string]any{
		"number":           m.Version,
		"platform":         m.platform(),
		"created_at":       m.CreatedAt.UTC().Format(time.RFC3339Nano),
		"summary":          m.Summary,
		"description":      m.Description,
		"authors":          strings.Join(m.Authors, ", "),
		"licenses":         nonNil(m.Licenses),
		"metadata":         m.Metadata,
		"sha":              m.SHA256,
		"prerelease":       m.prerelease(),
		"ruby_version":     reqOrDefault(m.RequiredRuby),
		"rubygems_version": reqOrDefault(m.RequiredRubygems),
		"downloads_count":  0,
	}
}

func reqOrDefault(reqs []string) string {
	if len(reqs) == 0 {
		return ">= 0"
	}
	return strings.Join(reqs, ", ")
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// latestMeta prefers the newest release on the ruby platform.
func latestMeta(ms []*GemMeta) *GemMeta {
	var best *GemMeta
	score := func(m *GemMeta) int {
		s := 0
		if !m.prerelease() {
			s += 2
		}
		if m.platform() == "ruby" {
			s++
		}
		return s
	}
	for _, m := range ms {
		if best == nil {
			best = m
			continue
		}
		sb, sm := score(best), score(m)
		if sm > sb || (sm == sb && pkgbase.CompareVersions(m.Version, best.Version) > 0) {
			best = m
		}
	}
	return best
}

func dependencyRows(ms []*GemMeta) []map[string]any {
	rows := []map[string]any{}
	for _, m := range ms {
		deps := [][]string{}
		for _, d := range m.runtimeDeps() {
			deps = append(deps, []string{d.Name, strings.Join(d.Requirements, ", ")})
		}
		rows = append(rows, map[string]any{"name": m.Name, "number": m.Version, "platform": m.platform(), "dependencies": deps})
	}
	return rows
}

func dependencyMarshal(ms []*GemMeta) []byte {
	list := make([]any, 0, len(ms))
	for _, m := range ms {
		deps := []any{}
		for _, d := range m.runtimeDeps() {
			deps = append(deps, []any{d.Name, strings.Join(d.Requirements, ", ")})
		}
		list = append(list, rbHash{
			{rbSym("name"), m.Name},
			{rbSym("number"), m.Version},
			{rbSym("platform"), m.platform()},
			{rbSym("dependencies"), deps},
		})
	}
	return rubyMarshal(list)
}

func splitGemsParam(q string) []string {
	var out []string
	for _, n := range strings.Split(q, ",") {
		if n = strings.TrimSpace(n); validName(n) {
			out = append(out, n)
		}
	}
	return out
}

// specsFor selects the metas for a legacy index file.
func specsFor(file string, all []*GemMeta) []*GemMeta {
	file = strings.TrimSuffix(strings.TrimPrefix(file, "/"), ".gz")
	var out []*GemMeta
	switch file {
	case "prerelease_specs.4.8":
		for _, m := range all {
			if m.prerelease() {
				out = append(out, m)
			}
		}
	case "latest_specs.4.8":
		latest := map[string]*GemMeta{}
		var keys []string
		for _, m := range all {
			if m.prerelease() {
				continue
			}
			k := m.Name + "\x00" + m.platform()
			cur, ok := latest[k]
			if !ok {
				keys = append(keys, k)
			}
			if !ok || pkgbase.CompareVersions(m.Version, cur.Version) > 0 {
				latest[k] = m
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, latest[k])
		}
	default:
		for _, m := range all {
			if !m.prerelease() {
				out = append(out, m)
			}
		}
	}
	return out
}

func detailFromMetas(name string, ms []*GemMeta) *registry.PackageDetail {
	latest := latestMeta(ms)
	d := &registry.GenericPackageDetail{
		LatestVersion: latest.Version,
		Description:   firstNonEmpty(latest.Summary, latest.Description),
		Homepage:      latest.Homepage,
		License:       strings.Join(latest.Licenses, " OR "),
	}
	if len(latest.Authors) > 0 {
		d.Metadata = map[string]string{"authors": strings.Join(latest.Authors, ", ")}
	}
	byVersion := map[string][]*GemMeta{}
	for _, m := range ms {
		byVersion[m.Version] = append(byVersion[m.Version], m)
	}
	vs := uniqueVersions(ms)
	for i := len(vs) - 1; i >= 0; i-- {
		row := registry.GenericVersionDetail{Version: vs[i]}
		var platforms []string
		var published time.Time
		for _, m := range byVersion[vs[i]] {
			row.Files = append(row.Files, registry.GenericFile{Name: m.FullName() + ".gem", Size: m.Size, SHA256: m.SHA256})
			row.Size += m.Size
			platforms = append(platforms, m.platform())
			if !m.CreatedAt.IsZero() && (published.IsZero() || m.CreatedAt.Before(published)) {
				published = m.CreatedAt
			}
			if row.Metadata == nil && !isDefaultReq(m.RequiredRuby) {
				row.Metadata = map[string]string{"ruby_version": strings.Join(m.RequiredRuby, ", ")}
			}
		}
		if !published.IsZero() {
			row.PublishedAt = published.UTC().Format(time.RFC3339)
		}
		if row.Metadata == nil {
			row.Metadata = map[string]string{}
		}
		row.Metadata["platforms"] = strings.Join(platforms, ", ")
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}
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
		return &Local{Repo: base, store: newStore(base.Store)}, nil
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
	if !validName(name) {
		return nil, registry.ErrInvalidPackageName
	}
	ms, err := l.store.LoadMetas(name)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	return detailFromMetas(name, ms), nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	ms, err := l.store.matchVersion(ref.Name, ref.Version, "")
	if err != nil || len(ms) == 0 {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	m := ms[0]
	return registry.ArtifactMeta{License: strings.Join(m.Licenses, " OR "), PublishedAt: m.CreatedAt}, nil
}

// DeleteVersion removes name@version (all platforms, or one when
// version is "{version}-{platform}") and regenerates the indexes.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	ms, err := l.store.matchVersion(name, version, "")
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, m := range ms {
		if err := l.store.remove(m); err != nil {
			return err
		}
	}
	if err := l.store.Regenerate(name); err != nil {
		return err
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

// PromoteVersion copies name@version (every platform) into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("rubygems: promote target %s/%s is not a local rubygems repository", dst.Namespace(), dst.Name())
	}
	ms, err := l.store.matchVersion(name, version, "")
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, m := range ms {
		body, err := l.store.Read(gemRel(m.FullName()))
		if err != nil {
			return err
		}
		if code, err := d.Guard.Check(d.store.Store, d.store.Exists(metaRel(m.Name, m.versionPlatform())), int64(len(body))); err != nil {
			return fmt.Errorf("promote rejected (%d): %w", code, err)
		}
		if err := d.store.put(m, body); err != nil {
			return err
		}
		d.EmitPublished(m.FullName(), int64(len(body)))
	}
	return d.store.Regenerate(name)
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		l.serveRead(w, r)
	case http.MethodPost:
		if p == "/api/v1/gems" {
			l.push(w, r)
			return
		}
		pkgbase.NotFound(w)
	case http.MethodDelete:
		if p == "/api/v1/gems/yank" {
			l.yank(w, r)
			return
		}
		pkgbase.NotFound(w)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) push(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	m, err := parseGem(body)
	if err != nil {
		pkgbase.WriteBytes(w, r, http.StatusUnprocessableEntity, "text/plain", []byte(err.Error()))
		return
	}
	m.SHA256 = pkgbase.SHA256Hex(body)
	m.Size = int64(len(body))
	m.CreatedAt = time.Now().UTC()
	if !l.AllowPublish(w, l.store.Exists(metaRel(m.Name, m.versionPlatform())), m.Size) {
		return
	}
	if err := l.store.put(m, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.Regenerate(m.Name); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(m.FullName(), m.Size)
	pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte(fmt.Sprintf("Successfully registered gem: %s (%s)", m.Name, m.versionPlatform())))
}

func (l *Local) yank(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	vals := formValues(r, 1<<20)
	name, version, platform := vals.Get("gem_name"), vals.Get("version"), vals.Get("platform")
	if platform == "ruby" {
		platform = ""
	}
	if !validName(name) || !validVersion(version) {
		pkgbase.WriteBytes(w, r, http.StatusBadRequest, "text/plain", []byte("gem_name and version are required"))
		return
	}
	var ms []*GemMeta
	var err error
	if platform != "" {
		ms, err = l.store.matchVersion(name, version, platform)
	} else {
		// Without a platform, rubygems yanks the ruby-platform build.
		ms, err = l.store.matchVersion(name, version, "ruby")
		if err == nil && len(ms) == 0 {
			ms, err = l.store.matchVersion(name, version, "")
		}
	}
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(ms) == 0 {
		pkgbase.WriteBytes(w, r, http.StatusNotFound, "text/plain", []byte("The version "+version+" does not exist."))
		return
	}
	for _, m := range ms {
		if err := l.store.remove(m); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		l.EmitDeleted(m.FullName())
	}
	if err := l.store.Regenerate(name); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte(fmt.Sprintf("Successfully deleted gem: %s (%s)", name, version)))
}

func (l *Local) serveRead(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	s := l.store
	switch {
	case p == "/" || p == "":
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte("rubygems registry\n"))
	case p == "/versions":
		body, err := s.Read(localVersionsRel())
		if err != nil {
			body = []byte(versionsHeader())
		}
		serveCompact(w, r, body)
	case p == "/names":
		body, err := s.Read(localNamesRel())
		if err != nil {
			body = []byte("---\n")
		}
		serveCompact(w, r, body)
	case strings.HasPrefix(p, "/info/"):
		name, ok := singleSegment(p, "/info/", "")
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		body, err := s.Read(localInfoRel(name))
		if err != nil {
			pkgbase.NotFound(w)
			return
		}
		serveCompact(w, r, body)
	case strings.HasPrefix(p, "/gems/"):
		full, ok := gemFileFromPath(p)
		if !ok || !pkgbase.ServeStored(w, r, s.Store, gemRel(full), binaryType) {
			pkgbase.NotFound(w)
		}
	case strings.HasPrefix(p, "/quick/Marshal.4.8/"):
		full, ok := quickFromPath(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		name, version, platform, ok := splitGemFile(full)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		vp := version
		if platform != "" && platform != "ruby" {
			vp += "-" + platform
		}
		m, err := s.loadMeta(name, vp)
		if err != nil {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, binaryType, quickSpec(m))
	case isSpecsIndex(p):
		ms := specsFor(p, s.allMetas())
		body := specsIndex(ms)
		if !strings.HasSuffix(p, ".gz") {
			body = specsRaw(ms)
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, binaryType, body)
	case p == "/api/v1/api_key" || p == "/api/v1/api_key.json":
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte(presentedToken(r)))
	case p == "/api/v1/dependencies" || p == "/api/v1/dependencies.json":
		var ms []*GemMeta
		for _, n := range splitGemsParam(r.URL.Query().Get("gems")) {
			got, _ := s.LoadMetas(n)
			ms = append(ms, got...)
		}
		if strings.HasSuffix(p, ".json") {
			pkgbase.WriteJSON(w, r, http.StatusOK, dependencyRows(ms))
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, binaryType, dependencyMarshal(ms))
	case strings.HasPrefix(p, "/api/v1/gems/"):
		name, ok := singleSegment(p, "/api/v1/gems/", ".json")
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		ms, _ := s.LoadMetas(name)
		if len(ms) == 0 {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, gemJSON(pkgbase.PublicBase(r), latestMeta(ms)))
	case strings.HasPrefix(p, "/api/v1/versions/"):
		name, ok := singleSegment(p, "/api/v1/versions/", ".json")
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		ms, _ := s.LoadMetas(name)
		if len(ms) == 0 {
			pkgbase.NotFound(w)
			return
		}
		rows := make([]map[string]any, 0, len(ms))
		for i := len(ms) - 1; i >= 0; i-- {
			rows = append(rows, versionJSON(ms[i]))
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, rows)
	default:
		pkgbase.NotFound(w)
	}
}

// ── Virtual ──

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := pkgbase.NewVirtual(typ, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v}, nil
	}
}
