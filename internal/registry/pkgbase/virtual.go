package pkgbase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalJSON encodes v without HTML escaping.
func MarshalJSON(v any) ([]byte, error) { return marshalJSON(v) }

// Virtual is the default first-hit virtual repo. Protocols whose
// index documents must be merged across members embed it and
// override ServeHTTP.
type Virtual struct {
	*virtualbase.Base
	RepoType string
}

// NewVirtualFactory returns a factory building a first-hit Virtual.
func NewVirtualFactory(typ string, resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := NewVirtual(typ, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
}

// NewVirtual builds a Virtual (validating members).
func NewVirtual(typ string, resolver virtualbase.Resolver, ns string, r *service.RegistryRepository) (*Virtual, error) {
	if len(r.Members) == 0 {
		return nil, fmt.Errorf("%s/virtual %s/%s: members required", typ, ns, r.Name)
	}
	return &Virtual{Base: virtualbase.New(ns, r.Name, r.Members, resolver), RepoType: typ}, nil
}

func (v *Virtual) Type() string { return v.RepoType }

func (v *Virtual) PackageDetail(ctx context.Context, name string) (*registry.PackageDetail, error) {
	return v.DelegatePackageDetail(ctx, name)
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !IsRead(r) {
		Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	if !v.ServeFirstHit(w, r) {
		NotFound(w)
	}
}

// ListPackages unions member listings.
func (v *Virtual) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	merged := map[string]map[string]struct{}{}
	var order []string
	v.ForEachMember(func(reg registry.Registry) bool {
		l, ok := reg.(registry.PackageLister)
		if !ok {
			return false
		}
		pkgs, err := l.ListPackages(ctx)
		if err != nil {
			return false
		}
		for _, p := range pkgs {
			set, ok := merged[p.Name]
			if !ok {
				set = map[string]struct{}{}
				merged[p.Name] = set
				order = append(order, p.Name)
			}
			for _, ver := range p.Versions {
				set[ver] = struct{}{}
			}
		}
		return false
	})
	out := make([]registry.PackageSummary, 0, len(order))
	for _, name := range order {
		vs := make([]string, 0, len(merged[name]))
		for ver := range merged[name] {
			vs = append(vs, ver)
		}
		SortVersions(vs)
		out = append(out, registry.PackageSummary{Name: name, Versions: vs})
	}
	return out, nil
}

// CollectMembers runs fn against every member's recorded response for
// r and returns the 200 bodies (used by index-merging virtuals).
func (v *Virtual) CollectMembers(r *http.Request) [][]byte {
	var out [][]byte
	v.ForEachMember(func(reg registry.Registry) bool {
		if _, _, ok := registry.CheckGate(reg, r); !ok {
			return false
		}
		rec := newRecorder()
		reg.ServeHTTP(rec, r)
		if rec.code == http.StatusOK {
			out = append(out, rec.buf.Bytes())
		}
		return false
	})
	return out
}

type recorder struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func newRecorder() *recorder { return &recorder{hdr: http.Header{}, code: http.StatusOK} }

func (r *recorder) Header() http.Header         { return r.hdr }
func (r *recorder) WriteHeader(c int)           { r.code = c }
func (r *recorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
