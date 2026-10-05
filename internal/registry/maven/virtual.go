package maven

import (
	"context"
	"net/http"
	"path"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

var (
	_ registry.PackageLister      = (*Virtual)(nil)
	_ registry.ArtifactClassifier = (*Virtual)(nil)
)

// Virtual aggregates member repositories: first hit for artifacts,
// merged maven-metadata.xml (and its checksums) across members.
type Virtual struct{ *pkgbase.Virtual }

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := pkgbase.NewVirtual(service.RegistryTypeMaven, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v}, nil
	}
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		http.Error(w, "virtual repositories are read-only; publish to a local member", http.StatusMethodNotAllowed)
		return
	}
	rel := cleanRel(r.URL.Path)
	base, algo := splitChecksum(rel)
	if path.Base(base) == metadataFile {
		v.serveMetadata(w, r, rel, base, algo)
		return
	}
	if !v.ServeFirstHit(w, r) {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (v *Virtual) serveMetadata(w http.ResponseWriter, r *http.Request, rel, base, algo string) {
	req := r.Clone(r.Context())
	req.Method = http.MethodGet
	req.URL.Path = "/" + base
	req.URL.RawPath = ""
	bodies := v.CollectMembers(req)
	if len(bodies) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	merged := bodies[0]
	if len(bodies) > 1 {
		if m, ok := mergeMetadata(bodies); ok {
			merged = m
		}
	}
	if algo != "" {
		writeBody(w, r, rel, []byte(checksumHex(algo, merged)))
		return
	}
	writeBody(w, r, rel, merged)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (v *Virtual) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}
