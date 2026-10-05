package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/service"
)

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/.well-known/terraform.json")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{Store: base.Store, remote: true}}, nil
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

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, metaDir), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

// ArtifactInfo reports the upstream publish time of a module version.
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	kind, parts, ok := parseName(ref.Name)
	if !ok || kind != modulesRoot || !validSegment(ref.Version) {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	body, err := rr.moduleDoc(ctx, append(parts, ref.Version), false)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	var doc struct {
		PublishedAt time.Time `json:"published_at"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	return registry.ArtifactMeta{PublishedAt: doc.PublishedAt}, nil
}

// endpoint returns the absolute upstream URL of a discovered service
// ("modules.v1" / "providers.v1"), always ending in "/".
func (rr *Remote) endpoint(ctx context.Context, key string) string {
	fallback := map[string]string{"modules.v1": "/v1/modules/", "providers.v1": "/v1/providers/"}[key]
	discovery := rr.Client.BaseURL() + "/.well-known/terraform.json"
	val := fallback
	if body, err := rr.FetchCached(ctx, path.Join(metaDir, "discovery.json"), discovery, true); err == nil {
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			if s, ok := doc[key].(string); ok && s != "" {
				val = s
			}
		}
	}
	base, err := url.Parse(discovery)
	if err != nil {
		return rr.Client.BaseURL() + fallback
	}
	ref, err := url.Parse(val)
	if err != nil {
		ref, _ = url.Parse(fallback)
	}
	out := base.ResolveReference(ref).String()
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/.well-known/terraform.json":
		serveWellKnown(w, r)
	case strings.HasPrefix(p, "/v1/modules"):
		rr.serveModules(w, r)
	case strings.HasPrefix(p, "/v1/providers/"):
		rr.serveProviders(w, r)
	case strings.HasPrefix(p, "/archive/"):
		rr.serveArchive(w, r)
	default:
		pkgbase.NotFound(w)
	}
}

// ── modules ──

// moduleDoc proxies a JSON module document addressed by path segments
// below modules.v1.
func (rr *Remote) moduleDoc(ctx context.Context, parts []string, mutable bool) ([]byte, error) {
	cache := path.Join(append([]string{metaDir, modulesRoot}, parts...)...) + ".json"
	return rr.FetchCached(ctx, cache, rr.endpoint(ctx, "modules.v1")+strings.Join(parts, "/"), mutable)
}

func (rr *Remote) serveModules(w http.ResponseWriter, r *http.Request) {
	parts, ok := splitRest(r.URL.Path, "/v1/modules")
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	ctx := r.Context()
	switch {
	case len(parts) <= 1:
		upath := rr.endpoint(ctx, "modules.v1") + strings.Join(parts, "/")
		cache := path.Join(metaDir, "modules-list", strings.Join(parts, "_"), pkgbase.SHA256Hex([]byte(r.URL.RawQuery))+".json")
		if r.URL.RawQuery != "" {
			upath += "?" + r.URL.RawQuery
		}
		rr.ServeCached(w, r, cache, upath, "application/json", true)
	case len(parts) == 4 && parts[3] == "download":
		latest, err := rr.latestModule(ctx, parts[:3])
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		w.Header().Set("Location", pkgbase.Prefix(r)+"/v1/modules/"+strings.Join(parts[:3], "/")+"/"+latest+"/download")
		w.WriteHeader(http.StatusFound)
	case len(parts) == 5 && parts[4] == "download":
		loc, err := rr.moduleLocation(ctx, parts[:4])
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		w.Header().Set("X-Terraform-Get", loc.public(pkgbase.PublicBase(r), parts))
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3, len(parts) == 4:
		body, err := rr.moduleDoc(ctx, parts, true)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", body)
	default:
		pkgbase.NotFound(w)
	}
}

func parseModuleVersions(body []byte) []string {
	var doc struct {
		Modules []struct {
			Versions []struct {
				Version string `json:"version"`
			} `json:"versions"`
		} `json:"modules"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	var out []string
	for _, m := range doc.Modules {
		for _, v := range m.Versions {
			if v.Version != "" {
				out = append(out, v.Version)
			}
		}
	}
	return out
}

func (rr *Remote) latestModule(ctx context.Context, parts []string) (string, error) {
	body, err := rr.moduleDoc(ctx, append(append([]string(nil), parts...), "versions"), true)
	if err != nil {
		return "", err
	}
	latest := pkgbase.Latest(parseModuleVersions(body))
	if latest == "" {
		return "", fmt.Errorf("module has no versions: %w", registry.ErrPackageNotFound)
	}
	return latest, nil
}

// moduleLoc is the cached resolution of an upstream X-Terraform-Get.
type moduleLoc struct {
	Original string `json:"original"`
	// Archive is the http(s) archive URL kutu caches ("" = pass through).
	Archive string `json:"archive,omitempty"`
	Ext     string `json:"ext,omitempty"`
	Subdir  string `json:"subdir,omitempty"`
}

func (l moduleLoc) public(base string, parts []string) string {
	if l.Archive == "" {
		return l.Original
	}
	u := moduleArchiveURL(base, parts[0], parts[1], parts[2], parts[3], l.Ext)
	if l.Subdir != "" {
		u += "//" + l.Subdir
	}
	return u
}

// classifyLocation decides whether a go-getter source is an http(s)
// archive kutu can cache.
func classifyLocation(raw string) moduleLoc {
	loc := moduleLoc{Original: raw}
	if strings.Contains(raw, "::") {
		return loc
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return loc
	}
	if i := strings.Index(u.Path, "//"); i >= 0 {
		loc.Subdir = strings.Trim(u.Path[i+2:], "/")
		u.Path = u.Path[:i]
		u.RawPath = ""
	}
	q := u.Query()
	ext := ""
	switch a := q.Get("archive"); {
	case a == "tar.gz" || a == "tgz":
		ext = "tar.gz"
	case a == "zip":
		ext = "zip"
	case a != "":
		return loc
	case strings.HasSuffix(u.Path, ".tar.gz") || strings.HasSuffix(u.Path, ".tgz"):
		ext = "tar.gz"
	case strings.HasSuffix(u.Path, ".zip"):
		ext = "zip"
	default:
		return loc
	}
	q.Del("archive")
	u.RawQuery = q.Encode()
	loc.Archive, loc.Ext = u.String(), ext
	return loc
}

func (rr *Remote) moduleLocation(ctx context.Context, parts []string) (moduleLoc, error) {
	rel := path.Join(moduleDirOf(parts), locationFile)
	if b, err := rr.store.Read(rel); err == nil {
		var loc moduleLoc
		if json.Unmarshal(b, &loc) == nil && loc.Original != "" {
			return loc, nil
		}
	}
	durl := rr.endpoint(ctx, "modules.v1") + strings.Join(parts, "/") + "/download"
	resp, err := rr.Router.For(durl).Get(ctx, durl)
	if err != nil {
		return moduleLoc{}, err
	}
	defer resp.Body.Close()
	raw := resp.Header.Get("X-Terraform-Get")
	if raw == "" {
		var doc struct {
			Location string `json:"location"`
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(body, &doc) == nil {
			raw = doc.Location
		}
	}
	if raw == "" {
		return moduleLoc{}, errors.New("upstream returned no module location")
	}
	if !strings.Contains(raw, "::") {
		if base, err := url.Parse(durl); err == nil {
			if ref, err := url.Parse(raw); err == nil && ref.Scheme == "" && (strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, ".")) {
				raw = base.ResolveReference(ref).String()
			}
		}
	}
	loc := classifyLocation(raw)
	if b, err := pkgbase.MarshalJSON(loc); err == nil {
		_ = rr.store.Write(rel, b)
	}
	return loc, nil
}

// ── providers ──

type providerDownload struct {
	Protocols           []string        `json:"protocols"`
	OS                  string          `json:"os"`
	Arch                string          `json:"arch"`
	Filename            string          `json:"filename"`
	DownloadURL         string          `json:"download_url"`
	SHASumsURL          string          `json:"shasums_url"`
	SHASumsSignatureURL string          `json:"shasums_signature_url"`
	SHASum              string          `json:"shasum"`
	SigningKeys         json.RawMessage `json:"signing_keys"`
}

func (rr *Remote) providerVersionsDoc(ctx context.Context, ns, ptype string) ([]byte, error) {
	return rr.FetchCached(ctx, path.Join(metaDir, providersRoot, ns, ptype, "versions.json"),
		rr.endpoint(ctx, "providers.v1")+ns+"/"+ptype+"/versions", true)
}

func (rr *Remote) providerDoc(ctx context.Context, ns, ptype, ver, osName, arch string) (*providerDownload, error) {
	rel := path.Join(providerDir(ns, ptype, ver), "download_"+osName+"_"+arch+".json")
	body, err := rr.FetchCached(ctx, rel, rr.endpoint(ctx, "providers.v1")+strings.Join([]string{ns, ptype, ver, "download", osName, arch}, "/"), false)
	if err != nil {
		return nil, err
	}
	var d providerDownload
	if err := json.Unmarshal(body, &d); err != nil {
		_ = rr.store.Delete(rel)
		return nil, fmt.Errorf("upstream provider document: %w", err)
	}
	return &d, nil
}

func (rr *Remote) serveProviders(w http.ResponseWriter, r *http.Request) {
	parts, ok := splitRest(r.URL.Path, "/v1/providers/")
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	ctx := r.Context()
	switch {
	case len(parts) == 3 && parts[2] == "versions":
		body, err := rr.providerVersionsDoc(ctx, parts[0], parts[1])
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", body)
	case len(parts) == 6 && parts[3] == "download":
		ns, ptype, ver := parts[0], parts[1], parts[2]
		d, err := rr.providerDoc(ctx, ns, ptype, ver, parts[4], parts[5])
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		out := *d
		base := providerArchiveBase(pkgbase.PublicBase(r), ns, ptype, ver)
		out.DownloadURL = base + "/" + d.Filename
		out.SHASumsURL = base + "/" + sumsName(ptype, ver)
		out.SHASumsSignatureURL = base + "/" + sumsName(ptype, ver) + ".sig"
		pkgbase.WriteJSON(w, r, http.StatusOK, out)
	default:
		pkgbase.NotFound(w)
	}
}

// cachedProviderDocs returns every cached download document of a
// provider version.
func (rr *Remote) cachedProviderDocs(dir string) []providerDownload {
	files, _ := rr.store.ListFiles(dir)
	var out []providerDownload
	for _, f := range files {
		if !strings.HasPrefix(f.Name, "download_") || !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		b, err := rr.store.Read(path.Join(dir, f.Name))
		if err != nil {
			continue
		}
		var d providerDownload
		if json.Unmarshal(b, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// get fetches an upstream URL. Requests to hosts other than the
// configured upstream (release CDNs, GitHub) go out without the
// repository's upstream credentials.
func (rr *Remote) get(ctx context.Context, src string) (io.ReadCloser, error) {
	if sameHost(src, rr.Client.BaseURL()) {
		resp, err := rr.Router.For(src).Get(ctx, src)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := foreignClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream GET %s: %w: %w", src, upstream.ErrTransient, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("upstream GET %s: %w", src, upstream.ErrNotFound)
		}
		return nil, fmt.Errorf("upstream GET %s status %d", src, resp.StatusCode)
	}
	return resp.Body, nil
}

var foreignClient = &http.Client{Timeout: 10 * time.Minute}

func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return ua.Scheme == "" || strings.EqualFold(ua.Host, ub.Host)
}

func (rr *Remote) fetchArtifact(ctx context.Context, rel, src, wantSHA string) ([]byte, error) {
	if rr.store.Exists(rel) {
		if b, err := rr.store.Read(rel); err == nil {
			return b, nil
		}
	}
	rc, err := rr.get(ctx, src)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if wantSHA != "" && !strings.EqualFold(pkgbase.SHA256Hex(body), wantSHA) {
		return nil, fmt.Errorf("upstream artifact %s: sha256 mismatch", path.Base(rel))
	}
	_ = rr.store.Write(rel, body)
	return body, nil
}

func (rr *Remote) serveArchive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if parts, ok := splitRest(r.URL.Path, "/archive/modules/"); ok && len(parts) == 4 {
		ver, ext := splitArchiveName(parts[3])
		if ver == "" {
			pkgbase.NotFound(w)
			return
		}
		mparts := []string{parts[0], parts[1], parts[2], ver}
		rel := path.Join(moduleDirOf(mparts), "module."+ext)
		if pkgbase.ServeStored(w, r, rr.store.Store, rel, archiveType(ext)) {
			return
		}
		loc, err := rr.moduleLocation(ctx, mparts)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		if loc.Archive == "" || loc.Ext != ext {
			pkgbase.NotFound(w)
			return
		}
		body, err := rr.fetchArtifact(ctx, rel, loc.Archive, "")
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, archiveType(ext), body)
		return
	}
	parts, ok := splitRest(r.URL.Path, "/archive/providers/")
	if !ok || len(parts) != 4 {
		pkgbase.NotFound(w)
		return
	}
	ns, ptype, ver, file := parts[0], parts[1], parts[2], parts[3]
	dir := providerDir(ns, ptype, ver)
	docs := rr.cachedProviderDocs(dir)
	var src, sha, ctype, rel string
	switch file {
	case sumsName(ptype, ver):
		rel, ctype = path.Join(dir, "SHA256SUMS"), "text/plain; charset=utf-8"
		for _, d := range docs {
			src = d.SHASumsURL
		}
	case sumsName(ptype, ver) + ".sig":
		rel, ctype = path.Join(dir, "SHA256SUMS.sig"), "application/octet-stream"
		for _, d := range docs {
			src = d.SHASumsSignatureURL
		}
	default:
		rel, ctype = path.Join(dir, file), "application/zip"
		for _, d := range docs {
			if d.Filename == file {
				src, sha = d.DownloadURL, d.SHASum
			}
		}
	}
	if pkgbase.ServeStored(w, r, rr.store.Store, rel, ctype) {
		return
	}
	if src == "" {
		pkgbase.NotFound(w)
		return
	}
	body, err := rr.fetchArtifact(ctx, rel, src, sha)
	if err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, ctype, body)
}

// Prefetch warms the version list and the (pinned or latest) version's
// artifacts: the module archive (when cacheable) or every provider
// platform zip plus SHA256SUMS and its signature.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	kind, parts, ok := parseName(name)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	if kind == modulesRoot {
		if version == "" {
			v, err := rr.latestModule(ctx, parts)
			if err != nil {
				return err
			}
			version = v
		}
		mparts := append(append([]string(nil), parts...), version)
		loc, err := rr.moduleLocation(ctx, mparts)
		if err != nil || loc.Archive == "" {
			return err
		}
		_, err = rr.fetchArtifact(ctx, path.Join(moduleDirOf(mparts), "module."+loc.Ext), loc.Archive, "")
		return err
	}
	ns, ptype := parts[0], parts[1]
	body, err := rr.providerVersionsDoc(ctx, ns, ptype)
	if err != nil {
		return err
	}
	var doc struct {
		Versions []struct {
			Version   string `json:"version"`
			Platforms []struct {
				OS   string `json:"os"`
				Arch string `json:"arch"`
			} `json:"platforms"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	if version == "" {
		var vs []string
		for _, v := range doc.Versions {
			vs = append(vs, v.Version)
		}
		version = pkgbase.Latest(vs)
	}
	dir := providerDir(ns, ptype, version)
	for _, v := range doc.Versions {
		if v.Version != version {
			continue
		}
		for i, p := range v.Platforms {
			d, err := rr.providerDoc(ctx, ns, ptype, version, p.OS, p.Arch)
			if err != nil {
				return err
			}
			if _, err := rr.fetchArtifact(ctx, path.Join(dir, d.Filename), d.DownloadURL, d.SHASum); err != nil {
				return err
			}
			if i == 0 {
				if _, err := rr.fetchArtifact(ctx, path.Join(dir, "SHA256SUMS"), d.SHASumsURL, ""); err != nil {
					return err
				}
				if _, err := rr.fetchArtifact(ctx, path.Join(dir, "SHA256SUMS.sig"), d.SHASumsSignatureURL, ""); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return registry.ErrPackageNotFound
}
