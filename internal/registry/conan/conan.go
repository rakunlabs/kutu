// Package conan implements a Conan 2 server (REST API v2, revisions
// enabled) compatible with conan_server / Artifactory:
//
//	GET    /v1/ping                                     capabilities probe
//	GET    /v2/users/authenticate                       login (echoes the token)
//	GET    /v2/users/check_credentials                  credential check
//	GET    /v2/conans/search?q=pattern                  recipe search
//	GET    /v2/conans/{n}/{v}/{u}/{c}/revisions         recipe revisions (newest first)
//	GET    /v2/conans/{n}/{v}/{u}/{c}/latest            latest recipe revision
//	GET    /v2/conans/{ref}/revisions/{rrev}/files[/{f}]  recipe files
//	PUT    /v2/conans/{ref}/revisions/{rrev}/files/{f}    upload recipe file
//	GET    /v2/conans/{ref}/revisions/{rrev}/search       binary package infos
//	GET    /v2/conans/{ref}/revisions/{rrev}/packages/{id}/revisions|latest
//	GET    /v2/conans/{ref}/revisions/{rrev}/packages/{id}/revisions/{prev}/files[/{f}]
//	PUT    /v2/conans/{ref}/revisions/{rrev}/packages/{id}/revisions/{prev}/files/{f}
//	DELETE /v2/conans/{ref}[/revisions/{rrev}[/packages[/{id}[/revisions/{prev}]]]]
//
// {ref} is {name}/{version}/{user}/{channel}; user and channel are "_"
// for references without them. Remote repos proxy a Conan 2 server
// (default https://center2.conan.io): revision lists, "latest" and
// searches honour MutableTTL, files under a revision are cached
// forever. Virtual repos merge revision lists and searches.
//
// Client configuration:
//
//	conan remote add kutu https://kutu.example.com/registries/{ns}/{repo}
//	conan remote login kutu x -p <token>
//	conan upload "pkg/*" -r kutu
package conan

import (
	"context"
	"crypto/sha1" //nolint:gosec
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
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeConan

// DefaultUpstream is the ConanCenter (Conan 2) remote.
const DefaultUpstream = "https://center2.conan.io"

const (
	dataDir      = "conans"
	searchDir    = "_search"
	revsFile     = "revisions.json"
	latestFile   = "latest.json"
	revFile      = "revision.json"
	filesFile    = "files.json"
	searchFile   = "search.json"
	capabilities = "complex_search,checksum_deploy,revisions"
	manifestFile = "conanmanifest.txt"
	ctOctet      = "application/octet-stream"
)

// ── References ──

// Ref is a Conan recipe reference; User/Channel are "_" when unset.
type Ref struct {
	Name, Version, User, Channel string
}

// String renders "name/version[@user/channel]".
func (r Ref) String() string {
	return r.Name + "/" + r.VersionKey()
}

// VersionKey renders "version[@user/channel]" (the listing version).
func (r Ref) VersionKey() string {
	if r.User == "_" && r.Channel == "_" {
		return r.Version
	}
	return r.Version + "@" + r.User + "/" + r.Channel
}

func (r Ref) urlPath() string {
	return "/v2/conans/" + r.Name + "/" + r.Version + "/" + r.User + "/" + r.Channel
}

// ParseVersionKey builds a Ref from a package name and a listing
// version ("1.2.3" or "1.2.3@user/channel").
func ParseVersionKey(name, key string) (Ref, bool) {
	ref := Ref{Name: name, Version: key, User: "_", Channel: "_"}
	if v, uc, ok := strings.Cut(key, "@"); ok {
		u, c, ok := strings.Cut(uc, "/")
		if !ok {
			return Ref{}, false
		}
		ref.Version, ref.User, ref.Channel = v, u, c
	}
	for _, s := range []string{ref.Name, ref.Version, ref.User, ref.Channel} {
		if !validSegment(s) {
			return Ref{}, false
		}
	}
	return ref, true
}

// ParseRef parses "name/version[@user/channel]".
func ParseRef(s string) (Ref, bool) {
	name, key, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Ref{}, false
	}
	if i := strings.IndexByte(key, '#'); i >= 0 {
		key = key[:i]
	}
	return ParseVersionKey(name, key)
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// ── Request parsing ──

type reqKind int

const (
	reqNone reqKind = iota
	reqPing
	reqAuth
	reqCheckCreds
	reqSearch
	reqRecipe
	reqRecipeRevs
	reqRecipeLatest
	reqRecipeRev
	reqRecipeFiles
	reqRecipeFile
	reqPkgSearch
	reqPackages
	reqPkg
	reqPkgRevs
	reqPkgLatest
	reqPkgRev
	reqPkgFiles
	reqPkgFile
)

type request struct {
	kind                    reqKind
	ref                     Ref
	rrev, pkgID, prev, file string
}

func parse(p string) request {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for _, s := range parts {
		if !validSegment(s) {
			return request{}
		}
	}
	api := parts[0] == "v1" || parts[0] == "v2"
	switch {
	case len(parts) == 1 && parts[0] == "ping", len(parts) == 2 && api && parts[1] == "ping":
		return request{kind: reqPing}
	case len(parts) == 3 && api && parts[1] == "users" && parts[2] == "authenticate":
		return request{kind: reqAuth}
	case len(parts) == 3 && api && parts[1] == "users" && parts[2] == "check_credentials":
		return request{kind: reqCheckCreds}
	case len(parts) < 3 || parts[0] != "v2" || parts[1] != "conans":
		return request{}
	case len(parts) == 3 && parts[2] == "search":
		return request{kind: reqSearch}
	case len(parts) < 6:
		return request{}
	}
	req := request{ref: Ref{Name: parts[2], Version: parts[3], User: parts[4], Channel: parts[5]}}
	rest := parts[6:]
	switch {
	case len(rest) == 0:
		req.kind = reqRecipe
		return req
	case len(rest) == 1 && rest[0] == "revisions":
		req.kind = reqRecipeRevs
		return req
	case len(rest) == 1 && rest[0] == "latest":
		req.kind = reqRecipeLatest
		return req
	case rest[0] != "revisions":
		return request{}
	}
	req.rrev, rest = rest[1], rest[2:]
	switch {
	case len(rest) == 0:
		req.kind = reqRecipeRev
	case rest[0] == "files" && len(rest) == 1:
		req.kind = reqRecipeFiles
	case rest[0] == "files" && len(rest) == 2:
		req.kind, req.file = reqRecipeFile, rest[1]
	case rest[0] == "search" && len(rest) == 1:
		req.kind = reqPkgSearch
	case rest[0] == "packages" && len(rest) == 1:
		req.kind = reqPackages
	case rest[0] == "packages":
		req.pkgID, rest = rest[1], rest[2:]
		switch {
		case len(rest) == 0:
			req.kind = reqPkg
		case len(rest) == 1 && rest[0] == "revisions":
			req.kind = reqPkgRevs
		case len(rest) == 1 && rest[0] == "latest":
			req.kind = reqPkgLatest
		case rest[0] != "revisions":
			return request{}
		case len(rest) == 2:
			req.kind, req.prev = reqPkgRev, rest[1]
		case len(rest) == 3 && rest[2] == "files":
			req.kind, req.prev = reqPkgFiles, rest[1]
		case len(rest) == 4 && rest[2] == "files":
			req.kind, req.prev, req.file = reqPkgFile, rest[1], rest[3]
		default:
			return request{}
		}
	default:
		return request{}
	}
	return req
}

// ── Revision lists ──

type revision struct {
	Revision string `json:"revision"`
	Time     string `json:"time,omitempty"`
}

type revisionList struct {
	Reference string     `json:"reference,omitempty"`
	Revisions []revision `json:"revisions"`
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00") }

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999-0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
}

func parseTime(s string) time.Time {
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func sortRevisions(revs []revision) {
	sort.SliceStable(revs, func(i, j int) bool { return parseTime(revs[i].Time).After(parseTime(revs[j].Time)) })
}

func mergeRevisions(lists ...[]revision) []revision {
	seen := map[string]bool{}
	var out []revision
	for _, l := range lists {
		for _, r := range l {
			if r.Revision == "" || seen[r.Revision] {
				continue
			}
			seen[r.Revision] = true
			out = append(out, r)
		}
	}
	sortRevisions(out)
	return out
}

func findRevision(revs []revision, id string) (revision, bool) {
	for _, r := range revs {
		if r.Revision == id {
			return r, true
		}
	}
	return revision{}, false
}

// ── Store ──

// Store wraps pkgbase.Store with the Conan layout:
//
//	conans/{n}/{v}/{u}/{c}/revisions.json
//	conans/{n}/{v}/{u}/{c}/{rrev}/files/{file}
//	conans/{n}/{v}/{u}/{c}/{rrev}/packages/{id}/revisions.json
//	conans/{n}/{v}/{u}/{c}/{rrev}/packages/{id}/{prev}/files/{file}
type Store struct{ *pkgbase.Store }

func (s *Store) recipeDir(ref Ref) string {
	return path.Join(dataDir, ref.Name, ref.Version, ref.User, ref.Channel)
}

func (s *Store) rrevDir(ref Ref, rrev string) string { return path.Join(s.recipeDir(ref), rrev) }

func (s *Store) recipeFilesDir(ref Ref, rrev string) string {
	return path.Join(s.rrevDir(ref, rrev), "files")
}

func (s *Store) pkgDir(ref Ref, rrev, id string) string {
	return path.Join(s.rrevDir(ref, rrev), "packages", id)
}

func (s *Store) prevDir(ref Ref, rrev, id, prev string) string {
	return path.Join(s.pkgDir(ref, rrev, id), prev)
}

func (s *Store) pkgFilesDir(ref Ref, rrev, id, prev string) string {
	return path.Join(s.prevDir(ref, rrev, id, prev), "files")
}

func (s *Store) readRevs(rel string) revisionList {
	var l revisionList
	if b, err := s.Read(rel); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	return l
}

func (s *Store) writeRevs(rel string, l revisionList) error {
	if len(l.Revisions) == 0 {
		return s.Delete(rel)
	}
	b, err := pkgbase.MarshalJSON(l)
	if err != nil {
		return err
	}
	return s.Write(rel, b)
}

// addRevision records id in the revision list at rel. bump moves an
// existing revision to the front with a fresh timestamp.
func (s *Store) addRevision(rel, reference, id string, bump bool) error {
	l := s.readRevs(rel)
	l.Reference = reference
	if _, ok := findRevision(l.Revisions, id); ok {
		if !bump {
			return nil
		}
		l.Revisions = removeRevision(l.Revisions, id)
	}
	l.Revisions = append([]revision{{Revision: id, Time: nowISO()}}, l.Revisions...)
	return s.writeRevs(rel, l)
}

func removeRevision(revs []revision, id string) []revision {
	out := revs[:0:0]
	for _, r := range revs {
		if r.Revision != id {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) dropRevision(rel, id string) error {
	l := s.readRevs(rel)
	l.Revisions = removeRevision(l.Revisions, id)
	return s.writeRevs(rel, l)
}

var errStop = errors.New("stop")

func (s *Store) hasFiles(rel string) bool {
	found := false
	_ = s.Walk(rel, func(string, rawfs.DirEntry) error {
		found = true
		return errStop
	})
	return found
}

// refsOf lists every stored reference of name.
func (s *Store) refsOf(name string) []Ref {
	var out []Ref
	versions, _ := s.ListDirs(path.Join(dataDir, name))
	for _, v := range versions {
		users, _ := s.ListDirs(path.Join(dataDir, name, v))
		for _, u := range users {
			channels, _ := s.ListDirs(path.Join(dataDir, name, v, u))
			for _, c := range channels {
				ref := Ref{Name: name, Version: v, User: u, Channel: c}
				if s.hasFiles(s.recipeDir(ref)) {
					out = append(out, ref)
				}
			}
		}
	}
	return out
}

func (s *Store) allRefs() []Ref {
	names, _ := s.ListDirs(dataDir)
	var out []Ref
	for _, n := range names {
		out = append(out, s.refsOf(n)...)
	}
	return out
}

// ListPackages returns name → "version[@user/channel]" rows.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := s.ListDirs(dataDir)
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(names))
	for _, n := range names {
		refs := s.refsOf(n)
		if len(refs) == 0 {
			continue
		}
		vs := make([]string, 0, len(refs))
		for _, r := range refs {
			vs = append(vs, r.VersionKey())
		}
		pkgbase.SortVersions(vs)
		out = append(out, registry.PackageSummary{Name: n, Versions: vs})
	}
	return out, nil
}

// recipeRevisions returns the known revisions of ref, newest first,
// falling back to cached "latest" and on-disk revision dirs.
func (s *Store) recipeRevisions(ref Ref) []revision {
	if l := s.readRevs(path.Join(s.recipeDir(ref), revsFile)); len(l.Revisions) > 0 {
		return l.Revisions
	}
	var latest revision
	if b, err := s.Read(path.Join(s.recipeDir(ref), latestFile)); err == nil && json.Unmarshal(b, &latest) == nil && latest.Revision != "" {
		return []revision{latest}
	}
	dirs, _ := s.ListDirs(s.recipeDir(ref))
	var out []revision
	for _, d := range dirs {
		if s.hasFiles(s.rrevDir(ref, d)) {
			out = append(out, revision{Revision: d})
		}
	}
	return out
}

func (s *Store) fileList(rel string) []registry.GenericFile {
	files, _ := s.ListFiles(rel)
	out := make([]registry.GenericFile, 0, len(files))
	for _, f := range files {
		out = append(out, registry.GenericFile{Name: f.Name, Size: f.Size})
	}
	return out
}

func (s *Store) packageCount(ref Ref, rrev string) int {
	ids, _ := s.ListDirs(path.Join(s.rrevDir(ref, rrev), "packages"))
	n := 0
	for _, id := range ids {
		if s.hasFiles(s.pkgDir(ref, rrev, id)) {
			n++
		}
	}
	return n
}

var (
	reLicense     = regexp.MustCompile(`(?m)^\s*license\s*=\s*["']([^"']+)["']`)
	reDescription = regexp.MustCompile(`(?m)^\s*description\s*=\s*["']([^"']+)["']`)
	reHomepage    = regexp.MustCompile(`(?m)^\s*homepage\s*=\s*["']([^"']+)["']`)
)

func attr(re *regexp.Regexp, src []byte) string {
	if m := re.FindSubmatch(src); m != nil {
		return string(m[1])
	}
	return ""
}

// DeleteVersion removes every revision of name@version.
func (s *Store) DeleteVersion(_ context.Context, name, version string) error {
	ref, ok := ParseVersionKey(name, version)
	if !ok {
		return registry.ErrPackageNotFound
	}
	if !s.hasFiles(s.recipeDir(ref)) {
		return registry.ErrPackageNotFound
	}
	if _, _, errs := s.DeleteTree(s.recipeDir(ref)); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	if !validSegment(name) {
		return nil, registry.ErrInvalidPackageName
	}
	refs := s.refsOf(name)
	if len(refs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if c := pkgbase.CompareVersions(refs[i].Version, refs[j].Version); c != 0 {
			return c > 0
		}
		return refs[i].VersionKey() < refs[j].VersionKey()
	})
	d := &registry.GenericPackageDetail{LatestVersion: refs[0].VersionKey()}
	for i, ref := range refs {
		revs := s.recipeRevisions(ref)
		row := registry.GenericVersionDetail{Version: ref.VersionKey(), Metadata: map[string]string{
			"reference": ref.String(),
			"revisions": strconv.Itoa(len(revs)),
		}}
		if len(revs) > 0 {
			rrev := revs[0].Revision
			row.Metadata["latest_revision"] = rrev
			row.Metadata["packages"] = strconv.Itoa(s.packageCount(ref, rrev))
			if t := parseTime(revs[0].Time); !t.IsZero() {
				row.PublishedAt = t.UTC().Format(time.RFC3339)
			}
			row.Files = s.fileList(s.recipeFilesDir(ref, rrev))
			for _, f := range row.Files {
				row.Size += f.Size
			}
			if i == 0 {
				if src, err := s.Read(path.Join(s.recipeFilesDir(ref, rrev), "conanfile.py")); err == nil {
					d.License = attr(reLicense, src)
					d.Description = attr(reDescription, src)
					d.Homepage = attr(reHomepage, src)
				}
			}
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	r, ok := ParseVersionKey(ref.Name, ref.Version)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	revs := s.recipeRevisions(r)
	if len(revs) == 0 {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	meta := registry.ArtifactMeta{PublishedAt: parseTime(revs[0].Time)}
	if src, err := s.Read(path.Join(s.recipeFilesDir(r, revs[0].Revision), "conanfile.py")); err == nil {
		meta.License = attr(reLicense, src)
	}
	return meta, nil
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := parse(r.URL.Path)
	switch p.kind {
	case reqNone, reqPing, reqAuth, reqCheckCreds, reqSearch:
		return registry.ArtifactRef{}, false
	case reqRecipeFile, reqPkgFile:
		return registry.ArtifactRef{Name: p.ref.Name, Version: p.ref.VersionKey()}, true
	}
	return registry.ArtifactRef{Name: p.ref.Name}, true
}

// ── Shared protocol handlers ──

func token(r *http.Request) string {
	if _, pw, ok := r.BasicAuth(); ok && pw != "" {
		return pw
	}
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return strings.TrimSpace(h)
}

// serveCommon handles ping / login endpoints; reports whether it did.
func serveCommon(w http.ResponseWriter, r *http.Request, p request) bool {
	switch p.kind {
	case reqPing:
		w.Header().Set("X-Conan-Server-Capabilities", capabilities)
		w.WriteHeader(http.StatusOK)
	case reqAuth:
		w.Header().Set("X-Conan-Server-Capabilities", capabilities)
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte(token(r)))
	case reqCheckCreds:
		user, _, ok := r.BasicAuth()
		if !ok || user == "" {
			user = "kutu"
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte(user))
	default:
		return false
	}
	return true
}

func globRE(pattern string, ignoreCase bool) *regexp.Regexp {
	var b strings.Builder
	if ignoreCase {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	for _, c := range pattern {
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile("^$")
	}
	return re
}

func searchRefs(s *Store, r *http.Request) []string {
	q := r.URL.Query().Get("q")
	if q == "" {
		q = "*"
	}
	ignoreCase := !strings.EqualFold(r.URL.Query().Get("ignorecase"), "false")
	re := globRE(q, ignoreCase)
	out := []string{}
	for _, ref := range s.allRefs() {
		full := ref.String()
		short := ref.Name + "/" + ref.Version
		if re.MatchString(full) || (!strings.Contains(q, "@") && re.MatchString(short)) {
			out = append(out, full)
		}
	}
	sort.Strings(out)
	return out
}

// loadBinaryInfo mirrors conan's load_binary_info for conaninfo.txt.
func loadBinaryInfo(text string) map[string]any {
	out := map[string]any{}
	section := ""
	var lines []string
	flush := func() {
		if section == "" {
			return
		}
		m := map[string]string{}
		for _, l := range lines {
			k, v, ok := strings.Cut(l, "=")
			if !ok {
				out[section] = lines
				return
			}
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		out[section] = m
	}
	for _, raw := range strings.Split(text, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			flush()
			section, lines = l[1:len(l)-1], nil
			continue
		}
		lines = append(lines, l)
	}
	flush()
	return out
}

func packageInfos(s *Store, ref Ref, rrev string) map[string]any {
	out := map[string]any{}
	ids, _ := s.ListDirs(path.Join(s.rrevDir(ref, rrev), "packages"))
	for _, id := range ids {
		revs := s.readRevs(path.Join(s.pkgDir(ref, rrev, id), revsFile)).Revisions
		if len(revs) == 0 {
			continue
		}
		info, err := s.Read(path.Join(s.pkgFilesDir(ref, rrev, id, revs[0].Revision), "conaninfo.txt"))
		if err != nil {
			out[id] = map[string]any{}
			continue
		}
		out[id] = loadBinaryInfo(string(info))
	}
	return out
}

func writeRevList(w http.ResponseWriter, r *http.Request, reference string, revs []revision) {
	if len(revs) == 0 {
		pkgbase.NotFound(w)
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, revisionList{Reference: reference, Revisions: revs})
}

func writeRev(w http.ResponseWriter, r *http.Request, revs []revision, id string) {
	if id == "" {
		if len(revs) == 0 {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, revs[0])
		return
	}
	rev, ok := findRevision(revs, id)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, rev)
}

func writeFiles(w http.ResponseWriter, r *http.Request, s *Store, rel string) {
	files, _ := s.ListFiles(rel)
	if len(files) == 0 {
		pkgbase.NotFound(w)
		return
	}
	m := make(map[string]map[string]any, len(files))
	for _, f := range files {
		m[f.Name] = map[string]any{}
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"files": m})
}

// serveLocalRead answers every GET from the local index.
func serveLocalRead(w http.ResponseWriter, r *http.Request, s *Store, p request) {
	ref := p.ref
	recipeRevs := func() []revision { return s.readRevs(path.Join(s.recipeDir(ref), revsFile)).Revisions }
	pkgRevs := func() []revision { return s.readRevs(path.Join(s.pkgDir(ref, p.rrev, p.pkgID), revsFile)).Revisions }
	switch p.kind {
	case reqSearch:
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"results": searchRefs(s, r)})
	case reqRecipeRevs:
		writeRevList(w, r, ref.String(), recipeRevs())
	case reqRecipeLatest:
		writeRev(w, r, recipeRevs(), "")
	case reqRecipeRev:
		writeRev(w, r, recipeRevs(), p.rrev)
	case reqRecipeFiles:
		writeFiles(w, r, s, s.recipeFilesDir(ref, p.rrev))
	case reqRecipeFile:
		if !pkgbase.ServeStored(w, r, s.Store, path.Join(s.recipeFilesDir(ref, p.rrev), p.file), ctOctet) {
			pkgbase.NotFound(w)
		}
	case reqPkgSearch:
		if _, ok := findRevision(recipeRevs(), p.rrev); !ok {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, packageInfos(s, ref, p.rrev))
	case reqPkgRevs:
		writeRevList(w, r, ref.String()+"#"+p.rrev+":"+p.pkgID, pkgRevs())
	case reqPkgLatest:
		writeRev(w, r, pkgRevs(), "")
	case reqPkgRev:
		writeRev(w, r, pkgRevs(), p.prev)
	case reqPkgFiles:
		writeFiles(w, r, s, s.pkgFilesDir(ref, p.rrev, p.pkgID, p.prev))
	case reqPkgFile:
		if !pkgbase.ServeStored(w, r, s.Store, path.Join(s.pkgFilesDir(ref, p.rrev, p.pkgID, p.prev), p.file), ctOctet) {
			pkgbase.NotFound(w)
		}
	default:
		pkgbase.NotFound(w)
	}
}

// ── Upload helpers ──

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// receive streams the request body into rel after applying the
// publish guard. Returns the stored size.
func receive(w http.ResponseWriter, r *http.Request, repo *pkgbase.Repo, rel string, exists bool) (int64, bool) {
	max := repo.MaxUpload
	if max <= 0 {
		max = pkgbase.DefaultMaxUpload
	}
	if r.ContentLength < 0 {
		body, err := pkgbase.ReadBody(r.Body, max)
		if err != nil {
			pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
			return 0, false
		}
		if !repo.AllowPublish(w, exists, int64(len(body))) {
			return 0, false
		}
		if err := repo.Store.Write(rel, body); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return 0, false
		}
		return int64(len(body)), true
	}
	size := r.ContentLength
	if size > max {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %d bytes", max))
		return 0, false
	}
	if !repo.AllowPublish(w, exists, size) {
		return 0, false
	}
	var cw countingWriter
	err := repo.Store.WriteStream(rel, io.TeeReader(io.LimitReader(r.Body, size), &cw), size)
	if err == nil && cw.n != size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		_ = repo.Store.Delete(rel)
		pkgbase.Error(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return 0, false
	}
	return size, true
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

func (l *Local) DeleteVersion(ctx context.Context, name, version string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.store.DeleteVersion(ctx, name, version); err != nil {
		return err
	}
	l.EmitDeleted(name + "/" + version)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref)
}

// PromoteVersion copies every revision (recipe + binaries) of
// name@version into dst, merging revision lists.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("conan: promote target %s/%s is not a conan local repository", dst.Namespace(), dst.Name())
	}
	ref, ok := ParseVersionKey(name, version)
	if !ok || !l.store.hasFiles(l.store.recipeDir(ref)) {
		return registry.ErrPackageNotFound
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return l.store.Walk(l.store.recipeDir(ref), func(rel string, _ rawfs.DirEntry) error {
		if path.Base(rel) == revsFile {
			src := l.store.readRevs(rel)
			cur := d.store.readRevs(rel)
			cur.Revisions = mergeRevisions(cur.Revisions, src.Revisions)
			if cur.Reference == "" {
				cur.Reference = src.Reference
			}
			return d.store.writeRevs(rel, cur)
		}
		rc, fi, err := l.store.Open(rel)
		if err != nil {
			return err
		}
		defer rc.Close()
		return d.store.WriteStream(rel, rc, fi.Size)
	})
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := parse(r.URL.Path)
	if serveCommon(w, r, p) {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveLocalRead(w, r, l.store, p)
	case http.MethodPut, http.MethodPost:
		l.upload(w, r, p)
	case http.MethodDelete:
		l.remove(w, r, p)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request, p request) {
	if p.kind != reqRecipeFile && p.kind != reqPkgFile {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !l.CheckPush(w) {
		return
	}
	s := l.store
	recipeRevsRel := path.Join(s.recipeDir(p.ref), revsFile)
	var rel string
	var exists bool
	if p.kind == reqRecipeFile {
		rel = path.Join(s.recipeFilesDir(p.ref, p.rrev), p.file)
		exists = s.Exists(rel)
		for _, rv := range s.readRevs(recipeRevsRel).Revisions {
			if rv.Revision != p.rrev {
				exists = true
			}
		}
	} else {
		rel = path.Join(s.pkgFilesDir(p.ref, p.rrev, p.pkgID, p.prev), p.file)
		exists = s.Exists(rel)
	}
	if strings.EqualFold(r.Header.Get("X-Checksum-Deploy"), "true") {
		// Dedup probe: 201 only when the identical file is already stored.
		want := strings.ToLower(r.Header.Get("X-Checksum-Sha1"))
		if b, err := s.Read(rel); err == nil && want != "" && sha1Hex(b) == want {
			w.WriteHeader(http.StatusCreated)
			return
		}
		pkgbase.NotFound(w)
		return
	}
	size, ok := receive(w, r, l.Repo, rel, exists)
	if !ok {
		return
	}
	l.mu.Lock()
	err := s.addRevision(recipeRevsRel, p.ref.String(), p.rrev, p.kind == reqRecipeFile && p.file == manifestFile)
	subject := p.ref.String() + "#" + p.rrev
	if err == nil && p.kind == reqPkgFile {
		pref := subject + ":" + p.pkgID
		err = s.addRevision(path.Join(s.pkgDir(p.ref, p.rrev, p.pkgID), revsFile), pref, p.prev, p.file == manifestFile)
		subject = pref + "#" + p.prev
	}
	l.mu.Unlock()
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(subject+"/"+p.file, size)
	w.WriteHeader(http.StatusCreated)
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request, p request) {
	if !l.CheckPush(w) {
		return
	}
	s := l.store
	l.mu.Lock()
	defer l.mu.Unlock()
	var tree, subject string
	var after func() error
	switch p.kind {
	case reqRecipe:
		tree, subject = s.recipeDir(p.ref), p.ref.String()
	case reqRecipeRev:
		tree, subject = s.rrevDir(p.ref, p.rrev), p.ref.String()+"#"+p.rrev
		after = func() error { return s.dropRevision(path.Join(s.recipeDir(p.ref), revsFile), p.rrev) }
	case reqPackages:
		tree, subject = path.Join(s.rrevDir(p.ref, p.rrev), "packages"), p.ref.String()+"#"+p.rrev+":*"
	case reqPkg:
		tree, subject = s.pkgDir(p.ref, p.rrev, p.pkgID), p.ref.String()+"#"+p.rrev+":"+p.pkgID
	case reqPkgRev:
		tree, subject = s.prevDir(p.ref, p.rrev, p.pkgID, p.prev), p.ref.String()+"#"+p.rrev+":"+p.pkgID+"#"+p.prev
		after = func() error { return s.dropRevision(path.Join(s.pkgDir(p.ref, p.rrev, p.pkgID), revsFile), p.prev) }
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.hasFiles(tree) {
		pkgbase.NotFound(w)
		return
	}
	if _, _, errs := s.DeleteTree(tree); len(errs) > 0 {
		pkgbase.Error(w, http.StatusInternalServerError, errs[0].Error())
		return
	}
	if after != nil {
		if err := after(); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	l.EmitDeleted(subject)
	w.WriteHeader(http.StatusOK)
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store    *Store
	inflight sync.Map
}

// newRemoteRepo mirrors pkgbase.NewRemoteRepo with a long client
// timeout (binary packages can be large).
func newRemoteRepo(deps registry.Deps, ns string, r *service.RegistryRepository) (*pkgbase.RemoteRepo, error) {
	b, err := upstream.BuildRemote(deps, typ+"/remote", ns, r, 5*time.Minute, upstream.RemoteBuildOptions{ClientTimeout: 30 * time.Minute})
	if err != nil {
		return nil, err
	}
	return &pkgbase.RemoteRepo{
		Repo: pkgbase.Repo{
			NS: ns, RepoName: r.Name, RepoType: typ, RepoKind: service.RegistryKindRemote,
			MaxUpload: r.MaxUploadSize, Emitter: deps.Emitter, Store: pkgbase.NewStore(b.FS, b.BasePath),
		},
		Client:     b.Client,
		Router:     upstream.NewRouter(b.Client, b.Upstreams),
		MutableTTL: b.MutableTTL,
		ProbePath:  "/v1/ping",
	}, nil
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		if r.URL == "" {
			cp := *r
			cp.URL = DefaultUpstream
			r = &cp
		}
		base, err := newRemoteRepo(deps, ns, r)
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
	return detail(rr.store, name)
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (rr *Remote) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(rr.store, ref)
}

var mutableFiles = map[string]bool{revsFile: true, latestFile: true, revFile: true, searchFile: true}

// PurgeCache drops revision lists, "latest" pointers and searches;
// opts.All removes every cached file.
func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	if opts.All {
		return rr.Purge(opts), nil
	}
	out := rr.Purge(opts, searchDir)
	_ = rr.store.Walk(dataDir, func(rel string, e rawfs.DirEntry) error {
		if !mutableFiles[path.Base(rel)] || path.Base(path.Dir(rel)) == "files" {
			out.Skipped++
			return nil
		}
		if err := rr.store.Delete(rel); err != nil {
			out.Errors = append(out.Errors, err.Error())
			return nil
		}
		out.PurgedFiles++
		out.PurgedBytes += e.Size
		return nil
	})
	return out, nil
}

func searchRel(rawQuery string) string {
	return path.Join(searchDir, pkgbase.SHA256Hex([]byte(rawQuery))[:32]+".json")
}

// cacheTarget maps a metadata request to its cache file and
// mutability. ok=false for file downloads and unknown paths.
func (rr *Remote) cacheTarget(r *http.Request, p request) (rel string, mutable, ok bool) {
	s := rr.store
	switch p.kind {
	case reqSearch:
		return searchRel(r.URL.RawQuery), true, true
	case reqRecipeRevs:
		return path.Join(s.recipeDir(p.ref), revsFile), true, true
	case reqRecipeLatest:
		return path.Join(s.recipeDir(p.ref), latestFile), true, true
	case reqRecipeRev:
		return path.Join(s.rrevDir(p.ref, p.rrev), revFile), true, true
	case reqRecipeFiles:
		return path.Join(s.rrevDir(p.ref, p.rrev), filesFile), false, true
	case reqPkgSearch:
		return path.Join(s.rrevDir(p.ref, p.rrev), searchFile), true, true
	case reqPkgRevs:
		return path.Join(s.pkgDir(p.ref, p.rrev, p.pkgID), revsFile), true, true
	case reqPkgLatest:
		return path.Join(s.pkgDir(p.ref, p.rrev, p.pkgID), latestFile), true, true
	case reqPkgRev:
		return path.Join(s.prevDir(p.ref, p.rrev, p.pkgID, p.prev), revFile), true, true
	case reqPkgFiles:
		return path.Join(s.prevDir(p.ref, p.rrev, p.pkgID, p.prev), filesFile), false, true
	}
	return "", false, false
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := parse(r.URL.Path)
	if serveCommon(w, r, p) {
		return
	}
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	upath := r.URL.EscapedPath()
	switch p.kind {
	case reqRecipeFile:
		rr.serveFile(w, r, path.Join(rr.store.recipeFilesDir(p.ref, p.rrev), p.file), upath)
		return
	case reqPkgFile:
		rr.serveFile(w, r, path.Join(rr.store.pkgFilesDir(p.ref, p.rrev, p.pkgID, p.prev), p.file), upath)
		return
	}
	rel, mutable, ok := rr.cacheTarget(r, p)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		upath += "?" + r.URL.RawQuery
	}
	rr.ServeCached(w, r, rel, upath, "application/json", mutable)
}

type lenientWriter struct {
	w      io.Writer
	failed bool
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	if !l.failed {
		if _, err := l.w.Write(p); err != nil {
			l.failed = true
		}
	}
	return len(p), nil
}

// fill fetches upath into rel. start (optional) is called once the
// upstream answered and returns the writer receiving a live copy.
func (rr *Remote) fill(ctx context.Context, rel, upath string, start func(*upstream.Response) io.Writer) error {
	client := rr.Client
	if rr.Router != nil {
		client = rr.Router.For(upath)
	}
	resp, err := client.Get(ctx, upath)
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
	if _, busy := rr.inflight.LoadOrStore(rel, struct{}{}); busy {
		_, err := io.Copy(sink, resp.Body)
		return err
	}
	defer rr.inflight.Delete(rel)
	if resp.ContentLength < 0 {
		body, err := io.ReadAll(io.TeeReader(resp.Body, sink))
		if err != nil {
			return err
		}
		return rr.store.Write(rel, body)
	}
	var cw countingWriter
	err = rr.store.WriteStream(rel, io.TeeReader(resp.Body, io.MultiWriter(sink, &cw)), resp.ContentLength)
	if err == nil && cw.n != resp.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		_ = rr.store.Delete(rel)
	}
	return err
}

func (rr *Remote) serveFile(w http.ResponseWriter, r *http.Request, rel, upath string) {
	if pkgbase.ServeStored(w, r, rr.store.Store, rel, ctOctet) {
		return
	}
	started := false
	err := rr.fill(context.WithoutCancel(r.Context()), rel, upath, func(resp *upstream.Response) io.Writer {
		started = true
		w.Header().Set("Content-Type", ctOctet)
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

// Prefetch warms the latest recipe revision of name@version (or of
// the newest version found by searching upstream) and its files.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if !validSegment(name) {
		return registry.ErrInvalidPackageName
	}
	var ref Ref
	if version == "" {
		q := "q=" + url.QueryEscape(name+"/*")
		body, err := rr.FetchCached(ctx, searchRel(q), "/v2/conans/search?"+q, true)
		if err != nil {
			return err
		}
		var res struct {
			Results []string `json:"results"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return err
		}
		var keys []string
		for _, s := range res.Results {
			if rf, ok := ParseRef(s); ok && rf.Name == name {
				keys = append(keys, rf.VersionKey())
			}
		}
		if version = pkgbase.Latest(keys); version == "" {
			return registry.ErrPackageNotFound
		}
	}
	ref, ok := ParseVersionKey(name, version)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	body, err := rr.FetchCached(ctx, path.Join(rr.store.recipeDir(ref), latestFile), ref.urlPath()+"/latest", true)
	if err != nil {
		return err
	}
	var latest revision
	if err := json.Unmarshal(body, &latest); err != nil || !validSegment(latest.Revision) {
		return fmt.Errorf("conan: bad latest revision for %s", ref)
	}
	base := ref.urlPath() + "/revisions/" + latest.Revision + "/files"
	body, err = rr.FetchCached(ctx, path.Join(rr.store.rrevDir(ref, latest.Revision), filesFile), base, false)
	if err != nil {
		return err
	}
	var files struct {
		Files map[string]any `json:"files"`
	}
	if err := json.Unmarshal(body, &files); err != nil {
		return err
	}
	for f := range files.Files {
		if !validSegment(f) {
			continue
		}
		rel := path.Join(rr.store.recipeFilesDir(ref, latest.Revision), f)
		if rr.store.Exists(rel) {
			continue
		}
		if err := rr.fill(ctx, rel, base+"/"+f, nil); err != nil {
			return err
		}
	}
	return nil
}

// ── Virtual ──

// Virtual merges revision lists and searches across members and
// serves everything else first-hit.
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
	p := parse(r.URL.Path)
	if serveCommon(w, r, p) {
		return
	}
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	switch p.kind {
	case reqRecipeRevs, reqPkgRevs:
		var lists [][]revision
		ref := ""
		for _, b := range v.CollectMembers(r) {
			var l revisionList
			if json.Unmarshal(b, &l) == nil {
				lists = append(lists, l.Revisions)
				if ref == "" {
					ref = l.Reference
				}
			}
		}
		writeRevList(w, r, ref, mergeRevisions(lists...))
	case reqRecipeLatest, reqPkgLatest:
		var all []revision
		for _, b := range v.CollectMembers(r) {
			var rv revision
			if json.Unmarshal(b, &rv) == nil && rv.Revision != "" {
				all = append(all, rv)
			}
		}
		writeRev(w, r, mergeRevisions(all), "")
	case reqSearch:
		seen := map[string]bool{}
		out := []string{}
		for _, b := range v.CollectMembers(r) {
			var res struct {
				Results []string `json:"results"`
			}
			if json.Unmarshal(b, &res) != nil {
				continue
			}
			for _, s := range res.Results {
				if !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
		sort.Strings(out)
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"results": out})
	case reqPkgSearch:
		bodies := v.CollectMembers(r)
		if len(bodies) == 0 {
			pkgbase.NotFound(w)
			return
		}
		merged := map[string]json.RawMessage{}
		for _, b := range bodies {
			var m map[string]json.RawMessage
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			for k, val := range m {
				if _, ok := merged[k]; !ok {
					merged[k] = val
				}
			}
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, merged)
	default:
		if !v.ServeFirstHit(w, r) {
			pkgbase.NotFound(w)
		}
	}
}
