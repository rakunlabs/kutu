package cargo

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

const (
	upstreamConfigRel = "upstream/config.json"
	defaultDL         = "/api/v1/crates/{crate}/{version}/download"
)

// Remote is a pull-through cache of an upstream sparse registry.
type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

// NewRemoteFactory builds remote cargo repositories.
func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/config.json")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: wrapStore(base.Store)}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return rr.store.listPackages()
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return packageDetail(rr.store, name)
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, "index", "upstream"), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (rr *Remote) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(rr.store, ref)
}

func (rr *Remote) client(p string) *upstream.Client {
	if rr.Router != nil {
		return rr.Router.For(p)
	}
	return rr.Client
}

// upstreamConfig returns the (cached) upstream config.json.
func (rr *Remote) upstreamConfig(ctx context.Context) (dl, api string) {
	body, err := rr.FetchCached(ctx, upstreamConfigRel, "/config.json", true)
	if err != nil {
		return defaultDL, ""
	}
	var cfg struct {
		DL  string `json:"dl"`
		API string `json:"api"`
	}
	if json.Unmarshal(body, &cfg) != nil || cfg.DL == "" {
		return defaultDL, cfg.API
	}
	return cfg.DL, cfg.API
}

// expandDL expands a config.json `dl` template for one crate version.
func expandDL(tmpl, name, version, cksum string) string {
	markers := []string{"{crate}", "{version}", "{prefix}", "{lowerprefix}", "{sha256-checksum}"}
	has := false
	for _, m := range markers {
		if strings.Contains(tmpl, m) {
			has = true
			break
		}
	}
	if !has {
		return strings.TrimRight(tmpl, "/") + "/" + name + "/" + version + "/download"
	}
	return strings.NewReplacer(
		"{crate}", name,
		"{version}", version,
		"{prefix}", prefixOf(name),
		"{lowerprefix}", strings.ToLower(prefixOf(name)),
		"{sha256-checksum}", cksum,
	).Replace(tmpl)
}

// fetchIndex returns the cached index file for idx, revalidating with
// the upstream (If-None-Match / If-Modified-Since) when stale.
func (rr *Remote) fetchIndex(ctx context.Context, idx string) ([]byte, error) {
	rel := indexRel(idx)
	cached, cerr := rr.store.Read(rel)
	if cerr == nil && rr.Fresh(rel) {
		return cached, nil
	}
	valRel := path.Join("upstream", "validators", idx+".json")
	var val struct {
		ETag         string `json:"etag,omitempty"`
		LastModified string `json:"last_modified,omitempty"`
	}
	hdr := http.Header{}
	if cerr == nil {
		if b, err := rr.store.Read(valRel); err == nil && json.Unmarshal(b, &val) == nil {
			if val.ETag != "" {
				hdr.Set("If-None-Match", val.ETag)
			}
			if val.LastModified != "" {
				hdr.Set("If-Modified-Since", val.LastModified)
			}
		}
	}
	resp, err := rr.client("/"+idx).GetWithHeaders(ctx, "/"+idx, hdr)
	if err != nil {
		if cerr == nil && !errors.Is(err, upstream.ErrNotFound) {
			return cached, nil
		}
		if errors.Is(err, upstream.ErrNotFound) {
			_ = rr.store.Delete(rel)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && cerr == nil {
		_ = rr.store.Write(rel, cached) // refresh TTL
		return cached, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = rr.store.Write(rel, body)
	val.ETag, val.LastModified = resp.ETag, resp.LastModified
	if b, err := pkgbase.MarshalJSON(val); err == nil {
		_ = rr.store.Write(valRel, b)
	}
	return body, nil
}

func (rr *Remote) fetchCrate(ctx context.Context, name, version string) ([]byte, error) {
	rel := crateRel(name, version)
	if b, err := rr.store.Read(rel); err == nil {
		return b, nil
	}
	var cksum string
	if idx := indexPath(name); idx != "" {
		if body, err := rr.fetchIndex(ctx, idx); err == nil {
			if ent, ok := findEntry(parseEntries(body), version); ok {
				cksum = ent.CKSum
				name = ent.Name
			}
		}
	}
	dl, _ := rr.upstreamConfig(ctx)
	target := expandDL(dl, name, version, cksum)
	resp, err := rr.client(target).Get(ctx, target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if cksum != "" && pkgbase.SHA256Hex(body) != cksum {
		return nil, fmt.Errorf("checksum mismatch for %s@%s", name, version)
	}
	_ = rr.store.Write(rel, body)
	return body, nil
}

// Prefetch warms the index file and the pinned (or latest unyanked) crate.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	idx := indexPath(name)
	if idx == "" {
		return registry.ErrInvalidPackageName
	}
	body, err := rr.fetchIndex(ctx, idx)
	if err != nil {
		return err
	}
	ents := parseEntries(body)
	if version == "" {
		version = maxVersion(ents)
	}
	if version == "" {
		return registry.ErrPackageNotFound
	}
	_, err = rr.fetchCrate(ctx, name, version)
	return err
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		cargoError(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/config.json":
		serveConfig(w, r)
	case p == "/api/v1/crates":
		rr.search(w, r)
	case strings.HasPrefix(p, "/api/v1/crates/"):
		name, version, ok := parseDownload(p)
		if !ok {
			cargoError(w, http.StatusNotFound, "no cargo route")
			return
		}
		if pkgbase.ServeStored(w, r, rr.store.Store, crateRel(name, version), "application/x-tar") {
			return
		}
		body, err := rr.fetchCrate(r.Context(), name, version)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/x-tar", body)
	default:
		idx := strings.ToLower(strings.Trim(p, "/"))
		if _, ok := parseIndexPath(idx); !ok {
			pkgbase.NotFound(w)
			return
		}
		body, err := rr.fetchIndex(r.Context(), idx)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		var mod time.Time
		if fi, err := rr.store.Stat(indexRel(idx)); err == nil {
			mod = fi.ModTime
		}
		serveIndexBytes(w, r, body, mod)
	}
}

// search proxies the upstream web API when config.json advertises one,
// falling back to the locally cached index.
func (rr *Remote) search(w http.ResponseWriter, r *http.Request) {
	if _, api := rr.upstreamConfig(r.Context()); api != "" {
		u := strings.TrimRight(api, "/") + "/api/v1/crates?" + url.Values{
			"q": {r.URL.Query().Get("q")}, "per_page": {fmt.Sprint(perPage(r))},
		}.Encode()
		if resp, err := rr.client(u).Get(r.Context(), u); err == nil {
			defer resp.Body.Close()
			var res searchResult
			if err := json.NewDecoder(resp.Body).Decode(&res); err == nil {
				if res.Crates == nil {
					res.Crates = []searchCrate{}
				}
				pkgbase.WriteJSON(w, r, http.StatusOK, res)
				return
			}
		}
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, rr.store.search(r.URL.Query().Get("q"), perPage(r)))
}
