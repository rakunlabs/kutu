package rubygems

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/service"
)

// Remote cache layout (base-relative):
//
//	cache/versions, cache/names, cache/info/{gem}   compact index (TTL)
//	cache/specs/{file}                              legacy indexes (TTL)
//	cache/api/...                                   JSON API (TTL)
//	gems/{full}.gem, quick/{full}.gemspec.rz        immutable
const cacheDir = "cache"

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/info/rake")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: newStore(base.Store)}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func remoteInfoRel(name string) string { return path.Join(cacheDir, "info", name) }

// cachedHeaders is the sidecar persisted next to streamed files.
type cachedHeaders struct {
	ETag       string `json:"etag"`
	ReprDigest string `json:"repr_digest"`
	Size       int64  `json:"size"`
}

func sidecarRel(rel string) string { return rel + ".hdr.json" }

func (rr *Remote) readSidecar(rel string) (*cachedHeaders, bool) {
	b, err := rr.store.Read(sidecarRel(rel))
	if err != nil {
		return nil, false
	}
	var h cachedHeaders
	if json.Unmarshal(b, &h) != nil {
		return nil, false
	}
	return &h, true
}

// fetchStream makes sure rel holds a (fresh, for mutable) copy of
// upstreamPath, spooling large bodies through a temp file instead of
// memory. A stale copy is kept when the upstream fails.
func (rr *Remote) fetchStream(ctx context.Context, rel, upstreamPath string, mutable bool) error {
	if rr.store.Exists(rel) && (!mutable || rr.Fresh(rel)) {
		return nil
	}
	client := rr.Client
	if rr.Router != nil {
		client = rr.Router.For(upstreamPath)
	}
	resp, err := client.Get(ctx, upstreamPath)
	if err != nil {
		if rr.store.Exists(rel) && !errors.Is(err, upstream.ErrNotFound) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp("", "kutu-rubygems-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	mh, sh := md5.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, mh, sh), resp.Body)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := rr.store.WriteStream(rel, tmp, n); err != nil {
		return err
	}
	hdr := cachedHeaders{
		ETag:       `"` + hex.EncodeToString(mh.Sum(nil)) + `"`,
		ReprDigest: "sha-256=:" + base64.StdEncoding.EncodeToString(sh.Sum(nil)) + ":",
		Size:       n,
	}
	b, _ := json.Marshal(hdr)
	_ = rr.store.Write(sidecarRel(rel), b)
	return nil
}

// serveStreamed serves a streamed cache entry with compact-index
// headers (Range supported: the headers always describe the full
// representation, which is what bundler validates appends against).
func (rr *Remote) serveStreamed(w http.ResponseWriter, r *http.Request, rel, upstreamPath, contentType string, mutable bool) {
	if err := rr.fetchStream(r.Context(), rel, upstreamPath, mutable); err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	if h, ok := rr.readSidecar(rel); ok {
		w.Header().Set("ETag", h.ETag)
		w.Header().Set("Repr-Digest", h.ReprDigest)
		if etagMatches(r, h.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if !pkgbase.ServeStored(w, r, rr.store.Store, rel, contentType) {
		pkgbase.NotFound(w)
	}
}

func (rr *Remote) fetchInfo(ctx context.Context, name string) ([]byte, error) {
	return rr.FetchCached(ctx, remoteInfoRel(name), "/info/"+name, true)
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/" || p == "":
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte("rubygems registry\n"))
	case p == "/versions":
		rr.serveStreamed(w, r, path.Join(cacheDir, "versions"), "/versions", compactType, true)
	case p == "/names":
		rr.serveStreamed(w, r, path.Join(cacheDir, "names"), "/names", compactType, true)
	case strings.HasPrefix(p, "/info/"):
		name, ok := singleSegment(p, "/info/", "")
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		body, err := rr.fetchInfo(r.Context(), name)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		serveCompact(w, r, body)
	case strings.HasPrefix(p, "/gems/"):
		full, ok := gemFileFromPath(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		rr.serveStreamed(w, r, gemRel(full), "/gems/"+full+".gem", binaryType, false)
	case strings.HasPrefix(p, "/quick/Marshal.4.8/"):
		full, ok := quickFromPath(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		rr.ServeCached(w, r, path.Join("quick", full+".gemspec.rz"), p, binaryType, false)
	case isSpecsIndex(p):
		file := strings.TrimPrefix(p, "/")
		rr.serveStreamed(w, r, path.Join(cacheDir, "specs", file), p, binaryType, true)
	case p == "/api/v1/api_key" || p == "/api/v1/api_key.json":
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain", []byte(presentedToken(r)))
	case p == "/api/v1/dependencies" || p == "/api/v1/dependencies.json":
		gems := splitGemsParam(r.URL.Query().Get("gems"))
		if len(gems) == 0 {
			if strings.HasSuffix(p, ".json") {
				pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", []byte("[]"))
			} else {
				pkgbase.WriteBytes(w, r, http.StatusOK, binaryType, rubyMarshal([]any{}))
			}
			return
		}
		q := strings.Join(gems, ",")
		ct := binaryType
		if strings.HasSuffix(p, ".json") {
			ct = "application/json"
		}
		key := pkgbase.SHA256Hex([]byte(q))
		rr.ServeCached(w, r, path.Join(cacheDir, "api", strings.TrimPrefix(p, "/api/v1/"), key), p+"?gems="+q, ct, true)
	case strings.HasPrefix(p, "/api/v1/gems/"):
		name, ok := singleSegment(p, "/api/v1/gems/", ".json")
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		body, err := rr.FetchCached(r.Context(), path.Join(cacheDir, "api", "gems", name+".json"), p, true)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", rewriteGemJSON(body, pkgbase.PublicBase(r)))
	case strings.HasPrefix(p, "/api/v1/versions/"):
		name, ok := singleSegment(p, "/api/v1/versions/", ".json")
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		rr.ServeCached(w, r, path.Join(cacheDir, "api", "versions", name+".json"), p, "application/json", true)
	default:
		pkgbase.NotFound(w)
	}
}

// rewriteGemJSON points gem_uri back at kutu and drops other
// upstream-hosted download links.
func rewriteGemJSON(body []byte, base string) []byte {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	name, _ := doc["name"].(string)
	version, _ := doc["version"].(string)
	platform, _ := doc["platform"].(string)
	if name != "" && version != "" {
		full := name + "-" + version
		if platform != "" && platform != "ruby" {
			full += "-" + platform
		}
		doc["gem_uri"] = base + "/gems/" + full + ".gem"
	}
	out, err := pkgbase.MarshalJSON(doc)
	if err != nil {
		return body
	}
	return out
}

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := rr.store.ListFiles(path.Join(cacheDir, "info"))
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, f := range names {
		if !validName(f.Name) || strings.HasSuffix(f.Name, ".json") {
			continue
		}
		body, err := rr.store.Read(remoteInfoRel(f.Name))
		if err != nil {
			continue
		}
		ms := metasFromInfo(f.Name, body)
		if len(ms) == 0 {
			continue
		}
		out = append(out, registry.PackageSummary{Name: f.Name, Versions: uniqueVersions(ms)})
	}
	return out, nil
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, cacheDir), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// PackageDetail is built from the cached compact index entry, enriched
// with the cached versions API document when present.
func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	if !validName(name) {
		return nil, registry.ErrInvalidPackageName
	}
	body, err := rr.store.Read(remoteInfoRel(name))
	if err != nil {
		return nil, registry.ErrPackageNotFound
	}
	ms := metasFromInfo(name, body)
	if len(ms) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	if vb, err := rr.store.Read(path.Join(cacheDir, "api", "versions", name+".json")); err == nil {
		enrichMetas(ms, vb)
	}
	for _, m := range ms {
		if fi, err := rr.store.Stat(gemRel(m.FullName())); err == nil {
			m.Size = fi.Size
		}
	}
	return detailFromMetas(name, ms), nil
}

// ArtifactInfo reads license / created_at from the upstream versions API.
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if !validName(ref.Name) {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	p := "/api/v1/versions/" + ref.Name + ".json"
	body, err := rr.FetchCached(ctx, path.Join(cacheDir, "api", "versions", ref.Name+".json"), p, true)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	var rows []apiVersion
	if json.Unmarshal(body, &rows) != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	for _, row := range rows {
		if row.Number != ref.Version && row.Number+"-"+row.Platform != ref.Version {
			continue
		}
		meta := registry.ArtifactMeta{License: strings.Join(row.Licenses, " OR ")}
		if t, err := time.Parse(time.RFC3339Nano, row.CreatedAt); err == nil {
			meta.PublishedAt = t
		}
		return meta, nil
	}
	return registry.ArtifactMeta{}, registry.ErrPackageNotFound
}

// Prefetch warms /info/{name} and the gem file of version (or the
// latest release on the ruby platform).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if !validName(name) {
		return registry.ErrInvalidPackageName
	}
	body, err := rr.fetchInfo(ctx, name)
	if err != nil {
		return err
	}
	ms := metasFromInfo(name, body)
	var targets []*GemMeta
	if version == "" {
		if m := latestMeta(ms); m != nil {
			targets = append(targets, m)
		}
	} else {
		for _, m := range ms {
			if m.Version == version || m.versionPlatform() == version {
				targets = append(targets, m)
			}
		}
	}
	if len(targets) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, m := range targets {
		full := m.FullName()
		if err := rr.fetchStream(ctx, gemRel(full), "/gems/"+full+".gem", false); err != nil {
			return err
		}
	}
	return nil
}

type apiVersion struct {
	Number    string   `json:"number"`
	Platform  string   `json:"platform"`
	CreatedAt string   `json:"created_at"`
	Licenses  []string `json:"licenses"`
	Summary   string   `json:"summary"`
	Authors   string   `json:"authors"`
}

func enrichMetas(ms []*GemMeta, body []byte) {
	var rows []apiVersion
	if json.Unmarshal(body, &rows) != nil {
		return
	}
	idx := map[string]apiVersion{}
	for _, row := range rows {
		idx[row.Number+"\x00"+row.Platform] = row
	}
	for _, m := range ms {
		row, ok := idx[m.Version+"\x00"+m.platform()]
		if !ok {
			continue
		}
		m.Licenses = row.Licenses
		m.Summary = row.Summary
		if row.Authors != "" {
			m.Authors = []string{row.Authors}
		}
		if t, err := time.Parse(time.RFC3339Nano, row.CreatedAt); err == nil {
			m.CreatedAt = t
		}
	}
}

// metasFromInfo parses compact index /info lines into GemMeta stubs.
func metasFromInfo(name string, info []byte) []*GemMeta {
	var out []*GemMeta
	for _, line := range infoLines(info) {
		tok, rest, _ := strings.Cut(line, " ")
		if tok == "" {
			continue
		}
		version, platform, _ := strings.Cut(tok, "-")
		m := &GemMeta{Name: name, Version: version, Platform: platform}
		deps, reqs, _ := strings.Cut(rest, "|")
		for _, d := range strings.Split(deps, ",") {
			dn, dr, ok := strings.Cut(strings.TrimSpace(d), ":")
			if ok && dn != "" {
				m.Dependencies = append(m.Dependencies, Dependency{Name: dn, Requirements: strings.Split(dr, "&"), Type: "runtime"})
			}
		}
		for _, kv := range strings.Split(reqs, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(kv), ":")
			switch k {
			case "checksum":
				m.SHA256 = v
			case "ruby":
				m.RequiredRuby = strings.Split(v, "&")
			case "rubygems":
				m.RequiredRubygems = strings.Split(v, "&")
			}
		}
		out = append(out, m)
	}
	sortMetas(out)
	return out
}
