// Package gitlfs implements a Git LFS server (Batch API, basic transfer
// adapter and the File Locking API):
//
//	POST   /objects/batch                    batch upload/download negotiation
//	PUT    /objects/{oid}                    upload (local, allow_push; sha256 + size verified)
//	GET    /objects/{oid}                    download (Range supported)
//	POST   /objects/verify                   post-upload verification
//	GET    /locks                            list locks (?path=, ?id=, ?cursor=, ?limit=)
//	POST   /locks                            create lock
//	POST   /locks/verify                     list ours/theirs locks
//	POST   /locks/{id}/unlock                release lock (force supported)
//
// Every route is also accepted under "/info/lfs" (and "{anything}.git/info/lfs")
// so both `lfs.url = {base}` and a git-remote-style URL work. Remote repos
// proxy another LFS server for downloads (objects are fetched through the
// upstream's batch API and cached forever); uploads and locks on remote and
// virtual repos are rejected.
//
// Client configuration:
//
//	git config lfs.url https://kutu.example.com/registries/{ns}/{repo}
//	git config credential.helper store   # username: x, password: <kutu token>
package gitlfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeGitLFS

// MediaType is the Git LFS JSON content type.
const MediaType = "application/vnd.git-lfs+json"

// actionTTL is the advertised lifetime of transfer actions.
const actionTTL = 3600

var oidRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidOID reports whether s is a sha256 hex object id.
func ValidOID(s string) bool { return oidRe.MatchString(s) }

func objectRel(oid string) string { return path.Join("objects", oid[0:2], oid[2:4], oid) }

// Store wraps pkgbase.Store with the LFS object layout.
type Store struct{ *pkgbase.Store }

// HasObject reports whether oid is stored (with the expected size when
// size >= 0) and returns its stored size.
func (s *Store) HasObject(oid string, size int64) (int64, bool) {
	if !ValidOID(oid) {
		return 0, false
	}
	fi, err := s.Stat(objectRel(oid))
	if err != nil || fi.IsDir {
		return 0, false
	}
	if size >= 0 && fi.Size != size {
		return fi.Size, false
	}
	return fi.Size, true
}

// ── Wire types ──

type batchObject struct {
	OID     string             `json:"oid"`
	Size    int64              `json:"size"`
	Authn   *bool              `json:"authenticated,omitempty"`
	Actions map[string]*action `json:"actions,omitempty"`
	Error   *objError          `json:"error,omitempty"`
}

type action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
}

type objError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type batchRequest struct {
	Operation string        `json:"operation"`
	Transfers []string      `json:"transfers,omitempty"`
	Ref       *refSpec      `json:"ref,omitempty"`
	Objects   []batchObject `json:"objects"`
	HashAlgo  string        `json:"hash_algo,omitempty"`
}

type refSpec struct {
	Name string `json:"name"`
}

type batchResponse struct {
	Transfer string        `json:"transfer"`
	Objects  []batchObject `json:"objects"`
	HashAlgo string        `json:"hash_algo,omitempty"`
}

// ── HTTP helpers ──

func lfsJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	body, err := pkgbase.MarshalJSON(v)
	if err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pkgbase.WriteBytes(w, r, code, MediaType, body)
}

func lfsError(w http.ResponseWriter, code int, msg string) {
	body, _ := pkgbase.MarshalJSON(map[string]string{"message": msg})
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// route strips the optional "…/info/lfs" prefix.
func route(p string) string {
	p = "/" + strings.Trim(p, "/")
	if i := strings.Index(p, "/info/lfs"); i >= 0 {
		rest := p[i+len("/info/lfs"):]
		if rest == "" || rest[0] == '/' {
			p = rest
		}
	}
	if p == "" {
		p = "/"
	}
	return p
}

func objectsOID(p string) (string, bool) {
	oid, ok := strings.CutPrefix(p, "/objects/")
	if !ok || !ValidOID(oid) {
		return "", false
	}
	return oid, true
}

func decodeBatch(w http.ResponseWriter, r *http.Request) (*batchRequest, bool) {
	var req batchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		lfsError(w, http.StatusUnprocessableEntity, "invalid batch request: "+err.Error())
		return nil, false
	}
	if req.Operation != "upload" && req.Operation != "download" {
		lfsError(w, http.StatusUnprocessableEntity, "unsupported operation "+req.Operation)
		return nil, false
	}
	if len(req.Transfers) > 0 {
		basic := false
		for _, t := range req.Transfers {
			basic = basic || t == "basic"
		}
		if !basic {
			lfsError(w, http.StatusUnprocessableEntity, "only the basic transfer adapter is supported")
			return nil, false
		}
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		lfsError(w, http.StatusConflict, "unsupported hash algorithm")
		return nil, false
	}
	return &req, true
}

// newAction builds a transfer action pointing back at kutu, echoing
// the caller's credentials so the client re-sends them.
func newAction(r *http.Request, href string) *action {
	a := &action{Href: href, ExpiresIn: actionTTL}
	if v := r.Header.Get("Authorization"); v != "" {
		a.Header = map[string]string{"Authorization": v}
	}
	return a
}

func objectHref(r *http.Request, oid string) string {
	return pkgbase.PublicBase(r) + "/objects/" + oid
}

// downloadObjects answers a download batch from a store.
func downloadObjects(r *http.Request, s *Store, objs []batchObject) []batchObject {
	out := make([]batchObject, 0, len(objs))
	for _, o := range objs {
		res := batchObject{OID: o.OID, Size: o.Size}
		switch {
		case !ValidOID(o.OID) || o.Size < 0:
			res.Error = &objError{Code: http.StatusUnprocessableEntity, Message: "invalid object"}
		default:
			if _, ok := s.HasObject(o.OID, o.Size); ok {
				res.Actions = map[string]*action{"download": newAction(r, objectHref(r, o.OID))}
			} else {
				res.Error = &objError{Code: http.StatusNotFound, Message: "object does not exist"}
			}
		}
		out = append(out, res)
	}
	return out
}

func serveObject(w http.ResponseWriter, r *http.Request, s *Store, oid string) bool {
	return pkgbase.ServeStored(w, r, s.Store, objectRel(oid), "application/octet-stream")
}

// ── Locks ──

type lockOwner struct {
	Name string `json:"name"`
}

// Lock is one file lock.
type Lock struct {
	ID       string    `json:"id"`
	Path     string    `json:"path"`
	LockedAt time.Time `json:"locked_at"`
	Owner    lockOwner `json:"owner"`
	Ref      string    `json:"-"`
}

const locksRel = "locks.json"

// lockStore is the minimal JSON-file backed lock table.
type lockStore struct {
	store *Store
}

func (ls lockStore) load() ([]Lock, error) {
	b, err := ls.store.Read(locksRel)
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var locks []Lock
	if err := json.Unmarshal(b, &locks); err != nil {
		return nil, err
	}
	return locks, nil
}

func (ls lockStore) save(locks []Lock) error {
	b, err := json.Marshal(locks)
	if err != nil {
		return err
	}
	return ls.store.Write(locksRel, b)
}

// owner returns a stable display name for the caller (the basic-auth
// username, else a short hash of the credential).
func owner(r *http.Request) string {
	if u, _, ok := r.BasicAuth(); ok && u != "" {
		return u
	}
	if v := r.Header.Get("Authorization"); v != "" {
		sum := sha256.Sum256([]byte(v))
		return "token-" + hex.EncodeToString(sum[:4])
	}
	return "anonymous"
}

func newLockID() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return hex.EncodeToString(sum[:8])
}

func pageLocks(locks []Lock, cursor string, limit int) ([]Lock, string) {
	start := 0
	if cursor != "" {
		for i, l := range locks {
			if l.ID == cursor {
				start = i
				break
			}
		}
	}
	locks = locks[start:]
	if limit > 0 && len(locks) > limit {
		return locks[:limit], locks[limit].ID
	}
	return locks, ""
}

// ── Virtual ──

// Virtual answers download batches from the first member holding each
// object and streams objects first-hit. Uploads and locks are rejected.
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

// objectHolder is implemented by local and remote repos.
type objectHolder interface {
	hasObject(ctx context.Context, oid string, size int64) bool
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := route(r.URL.Path)
	switch {
	case r.Method == http.MethodPost && p == "/objects/batch":
		req, ok := decodeBatch(w, r)
		if !ok {
			return
		}
		if req.Operation != "download" {
			lfsError(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; push to a local member")
			return
		}
		out := make([]batchObject, 0, len(req.Objects))
		for _, o := range req.Objects {
			res := batchObject{OID: o.OID, Size: o.Size}
			found := false
			if ValidOID(o.OID) {
				v.ForEachMember(func(reg registry.Registry) bool {
					if _, _, ok := registry.CheckGate(reg, r); !ok {
						return false
					}
					h, ok := reg.(objectHolder)
					found = ok && h.hasObject(r.Context(), o.OID, o.Size)
					return found
				})
			}
			if found {
				res.Actions = map[string]*action{"download": newAction(r, objectHref(r, o.OID))}
			} else {
				res.Error = &objError{Code: http.StatusNotFound, Message: "object does not exist"}
			}
			out = append(out, res)
		}
		lfsJSON(w, r, http.StatusOK, batchResponse{Transfer: "basic", Objects: out, HashAlgo: "sha256"})
	case pkgbase.IsRead(r):
		oid, ok := objectsOID(p)
		if !ok {
			lfsError(w, http.StatusNotFound, "not found")
			return
		}
		served := false
		v.ForEachMember(func(reg registry.Registry) bool {
			if _, _, ok := registry.CheckGate(reg, r); !ok {
				return false
			}
			if h, ok := reg.(objectHolder); ok && h.hasObject(r.Context(), oid, -1) {
				reg.ServeHTTP(w, r)
				served = true
			}
			return served
		})
		if !served {
			lfsError(w, http.StatusNotFound, "object does not exist")
		}
	default:
		lfsError(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; push to a local member")
	}
}
