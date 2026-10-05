package apt

import (
	"bytes"
	"context"
	"net/http"
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

// virtualTTL bounds how long a merged distribution is reused so the
// Release and the Packages files fetched right after it agree.
const virtualTTL = 30 * time.Second

// Virtual merges member Packages indexes into an unsigned Release and
// serves pool/ files first-hit.
type Virtual struct {
	*pkgbase.Virtual

	mu    sync.Mutex
	cache map[string]*mergedDist
}

type mergedDist struct {
	built time.Time
	files map[string][]byte // path below dists/{dist}/
}

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := pkgbase.NewVirtual(typ, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v, cache: map[string]*mergedDist{}}, nil
	}
}

func (v *Virtual) Stats(ctx context.Context) (registry.Stats, error) {
	var st registry.Stats
	if pkgs, err := v.ListPackages(ctx); err == nil {
		st.PackageCount = len(pkgs)
		for _, p := range pkgs {
			st.VersionCount += len(p.Versions)
		}
	}
	return st, nil
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	p := strings.Trim(path.Clean("/"+r.URL.Path), "/")
	if strings.HasPrefix(p, distsDir+"/") {
		dist, rest, ok := splitDist(p)
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		files := v.merged(r, dist)
		if b, ok := files[rest]; ok {
			pkgbase.WriteBytes(w, r, http.StatusOK, contentType(p), b)
			return
		}
		pkgbase.NotFound(w)
		return
	}
	if !v.ServeFirstHit(w, r) {
		pkgbase.NotFound(w)
	}
}

func (v *Virtual) merged(r *http.Request, dist string) map[string][]byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	if m, ok := v.cache[dist]; ok && time.Since(m.built) < virtualTTL {
		return m.files
	}
	files := v.build(r, dist)
	v.cache[dist] = &mergedDist{built: time.Now(), files: files}
	return files
}

// memberGet runs a GET for rel against every member and returns the
// 200 bodies in member order.
func (v *Virtual) memberGet(r *http.Request, rel string) [][]byte {
	sub := r.Clone(r.Context())
	sub.Method = http.MethodGet
	sub.URL.Path = "/" + rel
	sub.URL.RawPath = ""
	sub.Header.Del("Range")
	sub.Header.Del("If-Modified-Since")
	sub.Header.Del("If-None-Match")
	return v.CollectMembers(sub)
}

// memberPackages returns the decoded Packages bodies of comp/arch from
// every member, trying each compression in turn.
func (v *Virtual) memberPackages(r *http.Request, dist, comp, arch string) [][]byte {
	var out [][]byte
	v.ForEachMember(func(reg registry.Registry) bool {
		if _, _, ok := registry.CheckGate(reg, r); !ok {
			return false
		}
		for _, name := range []string{"Packages", "Packages.gz", "Packages.xz"} {
			rel := distsDir + "/" + dist + "/" + comp + "/binary-" + arch + "/" + name
			sub := r.Clone(r.Context())
			sub.Method = http.MethodGet
			sub.URL.Path = "/" + rel
			sub.URL.RawPath = ""
			sub.Header.Del("Range")
			rec := &bodyRecorder{hdr: http.Header{}, code: http.StatusOK}
			reg.ServeHTTP(rec, sub)
			if rec.code != http.StatusOK {
				continue
			}
			if raw, err := decompressAll(name, rec.buf.Bytes()); err == nil {
				out = append(out, raw)
				break
			}
		}
		return false
	})
	return out
}

func (v *Virtual) build(r *http.Request, dist string) map[string][]byte {
	compSet, archSet := map[string]bool{}, map[string]bool{}
	found := false
	for _, name := range []string{"Release", "InRelease"} {
		for _, b := range v.memberGet(r, distsDir+"/"+dist+"/"+name) {
			txt := releaseText(b)
			st := parseStanzas(txt)
			if len(st) == 0 {
				continue
			}
			found = true
			for _, c := range splitFields(st[0].Get("Components")) {
				compSet[c] = true
			}
			for _, a := range splitFields(st[0].Get("Architectures")) {
				if a != "all" && a != "source" {
					archSet[a] = true
				}
			}
		}
		if found {
			break
		}
	}
	if !found || len(compSet) == 0 {
		return nil
	}
	comps, arches := sortedKeys(compSet), sortedKeys(archSet)
	list := packagesFiles(comps, arches, func(c, a string, buf *bytes.Buffer) {
		seen := map[string]bool{}
		for _, body := range v.memberPackages(r, dist, c, a) {
			for _, s := range parseStanzas(body) {
				key := s.Get("Package") + "\x00" + s.Get("Version") + "\x00" + s.Get("Architecture")
				if seen[key] {
					continue
				}
				seen[key] = true
				s.render(buf)
				buf.WriteByte('\n')
			}
		}
	})
	sort.SliceStable(list, func(i, j int) bool { return list[i].path < list[j].path })
	files := make(map[string][]byte, len(list)+1)
	for _, f := range list {
		files[f.path] = f.data
	}
	files["Release"] = buildRelease(v.Name(), dist, arches, comps, list, time.Now())
	return files
}

type bodyRecorder struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func (r *bodyRecorder) Header() http.Header         { return r.hdr }
func (r *bodyRecorder) WriteHeader(c int)           { r.code = c }
func (r *bodyRecorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
