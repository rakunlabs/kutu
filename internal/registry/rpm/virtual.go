package rpm

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Virtual merges member repodata into one unsigned repository.
type Virtual struct {
	*pkgbase.Virtual

	mu   sync.Mutex
	key  string
	data *repodata
}

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
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	switch {
	case isRepoFile(p):
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain; charset=utf-8", repoFile(r, v.Namespace(), v.Name(), false))
		return
	case p == repomdAsc || isKeyPath(p):
		pkgbase.NotFound(w)
		return
	case p == repomdRel:
		rd := v.merged(r)
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/xml", rd.Repomd)
		return
	case strings.HasPrefix(p, repodataDir+"/"):
		v.mu.Lock()
		rd := v.data
		v.mu.Unlock()
		if rd == nil || rd.Files[p] == nil {
			rd = v.merged(r)
		}
		if b, ok := rd.Files[p]; ok {
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

// memberGet runs a GET for rel against reg, returning the 200 body.
func memberGet(reg registry.Registry, orig *http.Request, rel string) ([]byte, bool) {
	r := orig.Clone(orig.Context())
	r.Method = http.MethodGet
	r.URL = &url.URL{Path: "/" + strings.TrimPrefix(rel, "/")}
	r.RequestURI = r.URL.Path
	if _, _, ok := registry.CheckGate(reg, r); !ok {
		return nil, false
	}
	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		return nil, false
	}
	return rec.Body.Bytes(), true
}

type memberMeta struct {
	reg    registry.Registry
	repomd []byte
}

// merged returns the merged repodata, regenerating only when a
// member's repomd.xml changed.
func (v *Virtual) merged(r *http.Request) *repodata {
	var members []memberMeta
	var key bytes.Buffer
	v.ForEachMember(func(reg registry.Registry) bool {
		b, ok := memberGet(reg, r, repomdRel)
		if !ok {
			return false
		}
		members = append(members, memberMeta{reg: reg, repomd: b})
		key.WriteString(reg.Name())
		key.WriteByte(0)
		key.WriteString(sha256Hex(b))
		key.WriteByte(0)
		return false
	})
	k := key.String()
	v.mu.Lock()
	if v.data != nil && v.key == k {
		rd := v.data
		v.mu.Unlock()
		return rd
	}
	v.mu.Unlock()

	chunks := map[string][][]byte{}
	keepIDs := map[string]bool{}
	seenNEVRA := map[string]bool{}
	emitted := map[string]map[string]bool{"filelists": {}, "other": {}}
	for _, m := range members {
		hrefs, err := repomdHrefs(m.repomd)
		if err != nil {
			continue
		}
		docs := map[string][]byte{}
		for _, kind := range metaTypes {
			href := hrefs[kind]
			if href == "" {
				continue
			}
			raw, ok := memberGet(m.reg, r, href)
			if !ok {
				continue
			}
			if doc, err := decompress(raw); err == nil {
				docs[kind] = doc
			}
		}
		for _, c := range packageChunks(docs["primary"]) {
			id, err := parsePrimaryIdent(c)
			if err != nil || seenNEVRA[id.nevra()] || keepIDs[id.Checksum] {
				continue
			}
			seenNEVRA[id.nevra()] = true
			keepIDs[id.Checksum] = true
			chunks["primary"] = append(chunks["primary"], c)
		}
		for _, kind := range []string{"filelists", "other"} {
			for _, c := range packageChunks(docs[kind]) {
				if id := chunkPkgID(c); keepIDs[id] && !emitted[kind][id] {
					emitted[kind][id] = true
					chunks[kind] = append(chunks[kind], c)
				}
			}
		}
	}
	docs := map[string][]byte{}
	for _, kind := range metaTypes {
		docs[kind] = wrapDoc(kind, chunks[kind])
	}
	rd, err := buildRepodata(docs, time.Now())
	if err != nil {
		rd = &repodata{Files: map[string][]byte{}}
	}
	v.mu.Lock()
	v.key, v.data = k, rd
	v.mu.Unlock()
	return rd
}
