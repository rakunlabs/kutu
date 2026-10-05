package cargo

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Virtual merges sparse index files across members; downloads and
// API calls are first-hit.
type Virtual struct {
	*pkgbase.Virtual
}

// NewVirtualFactory builds virtual cargo repositories.
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
		cargoError(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/config.json":
		serveConfig(w, r)
	case p == "/api/v1/crates":
		v.search(w, r)
	case strings.HasPrefix(p, "/api/"):
		if !v.ServeFirstHit(w, r) {
			cargoError(w, http.StatusNotFound, "not found")
		}
	default:
		if _, ok := parseIndexPath(p); !ok {
			pkgbase.NotFound(w)
			return
		}
		// Members must return full bodies, not 304s.
		inner := r.Clone(r.Context())
		inner.Header.Del("If-None-Match")
		inner.Header.Del("If-Modified-Since")
		bodies := v.CollectMembers(inner)
		merged := mergeIndexes(bodies)
		if len(merged) == 0 {
			pkgbase.NotFound(w)
			return
		}
		serveIndexBytes(w, r, encodeEntries(merged), time.Time{})
	}
}

// mergeIndexes unions index entries; the first member wins per version.
func mergeIndexes(bodies [][]byte) []indexEntry {
	seen := map[string]bool{}
	var out []indexEntry
	for _, b := range bodies {
		for _, e := range parseEntries(b) {
			if seen[e.Version] {
				continue
			}
			seen[e.Version] = true
			out = append(out, e)
		}
	}
	sortEntries(out)
	return out
}

func (v *Virtual) search(w http.ResponseWriter, r *http.Request) {
	limit := perPage(r)
	res := searchResult{Crates: []searchCrate{}}
	seen := map[string]bool{}
	for _, b := range v.CollectMembers(r) {
		var part searchResult
		if json.Unmarshal(b, &part) != nil {
			continue
		}
		for _, c := range part.Crates {
			if seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			res.Crates = append(res.Crates, c)
		}
	}
	sort.SliceStable(res.Crates, func(i, j int) bool { return res.Crates[i].Name < res.Crates[j].Name })
	res.Meta.Total = len(res.Crates)
	if len(res.Crates) > limit {
		res.Crates = res.Crates[:limit]
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, res)
}
