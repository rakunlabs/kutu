package gitlfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Local is a Git LFS server backed by a raw mount.
type Local struct {
	*pkgbase.Repo
	store *Store
	locks lockStore
	mu    sync.Mutex
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		s := &Store{base.Store}
		return &Local{Repo: base, store: s, locks: lockStore{store: s}}, nil
	}
}

func (l *Local) Close() error  { return nil }
func (l *Local) Store() *Store { return l.store }

func (l *Local) hasObject(_ context.Context, oid string, size int64) bool {
	_, ok := l.store.HasObject(oid, size)
	return ok
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := route(r.URL.Path)
	switch {
	case p == "/objects/batch" && r.Method == http.MethodPost:
		l.batch(w, r)
	case p == "/objects/verify" && r.Method == http.MethodPost:
		verify(w, r, l.store)
	case strings.HasPrefix(p, "/locks"):
		l.serveLocks(w, r, p)
	case strings.HasPrefix(p, "/objects/"):
		oid, ok := objectsOID(p)
		if !ok {
			lfsError(w, http.StatusNotFound, "not found")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			if !serveObject(w, r, l.store, oid) {
				lfsError(w, http.StatusNotFound, "object does not exist")
			}
		case http.MethodPut:
			l.upload(w, r, oid)
		default:
			lfsError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		lfsError(w, http.StatusNotFound, "not found")
	}
}

func (l *Local) batch(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBatch(w, r)
	if !ok {
		return
	}
	if req.Operation == "download" {
		lfsJSON(w, r, http.StatusOK, batchResponse{Transfer: "basic", Objects: downloadObjects(r, l.store, req.Objects), HashAlgo: "sha256"})
		return
	}
	if !l.AllowPush {
		lfsError(w, http.StatusForbidden, "push disabled for this repository")
		return
	}
	max := l.MaxUpload
	if max <= 0 {
		max = pkgbase.DefaultMaxUpload
	}
	var incoming int64
	for _, o := range req.Objects {
		if _, ok := l.store.HasObject(o.OID, o.Size); !ok && o.Size > 0 {
			incoming += o.Size
		}
	}
	if code, err := l.Guard.Check(l.store.Store, false, incoming); code != 0 {
		lfsError(w, code, err.Error())
		return
	}
	out := make([]batchObject, 0, len(req.Objects))
	for _, o := range req.Objects {
		res := batchObject{OID: o.OID, Size: o.Size}
		switch {
		case !ValidOID(o.OID) || o.Size < 0:
			res.Error = &objError{Code: http.StatusUnprocessableEntity, Message: "invalid object"}
		case o.Size > max:
			res.Error = &objError{Code: http.StatusUnprocessableEntity, Message: fmt.Sprintf("object exceeds %d bytes", max)}
		default:
			if _, exists := l.store.HasObject(o.OID, o.Size); !exists {
				href := objectHref(r, o.OID)
				res.Actions = map[string]*action{
					"upload": newAction(r, href),
					"verify": newAction(r, pkgbase.PublicBase(r)+"/objects/verify"),
				}
			}
		}
		out = append(out, res)
	}
	lfsJSON(w, r, http.StatusOK, batchResponse{Transfer: "basic", Objects: out, HashAlgo: "sha256"})
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request, oid string) {
	if !l.AllowPush {
		lfsError(w, http.StatusForbidden, "push disabled for this repository")
		return
	}
	max := l.MaxUpload
	if max <= 0 {
		max = pkgbase.DefaultMaxUpload
	}
	if r.ContentLength > max {
		lfsError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("object exceeds %d bytes", max))
		return
	}
	if size, ok := l.store.HasObject(oid, -1); ok {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, max))
		if r.ContentLength >= 0 && r.ContentLength != size {
			lfsError(w, http.StatusBadRequest, "size mismatch")
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	tmp, err := os.CreateTemp("", "kutu-lfs-*")
	if err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r.Body, max+1))
	if err != nil {
		lfsError(w, http.StatusBadRequest, err.Error())
		return
	}
	if n > max {
		lfsError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("object exceeds %d bytes", max))
		return
	}
	if r.ContentLength >= 0 && n != r.ContentLength {
		lfsError(w, http.StatusBadRequest, "size mismatch")
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != oid {
		lfsError(w, http.StatusUnprocessableEntity, "sha256 mismatch: object content does not match oid")
		return
	}
	if !l.Guard.Allow(w, l.store.Store, false, n) {
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.WriteStream(objectRel(oid), tmp, n); err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(oid, n)
	w.WriteHeader(http.StatusOK)
}

func verify(w http.ResponseWriter, r *http.Request, s *Store) {
	var o batchObject
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&o); err != nil {
		lfsError(w, http.StatusUnprocessableEntity, "invalid verify request")
		return
	}
	if _, ok := s.HasObject(o.OID, o.Size); !ok {
		lfsError(w, http.StatusNotFound, "object does not exist")
		return
	}
	lfsJSON(w, r, http.StatusOK, map[string]any{"oid": o.OID, "size": o.Size})
}

// ── Locks ──

func (l *Local) serveLocks(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/locks" && r.Method == http.MethodGet:
		l.listLocks(w, r)
	case p == "/locks" && r.Method == http.MethodPost:
		l.createLock(w, r)
	case p == "/locks/verify" && r.Method == http.MethodPost:
		l.verifyLocks(w, r)
	case strings.HasPrefix(p, "/locks/") && strings.HasSuffix(p, "/unlock") && r.Method == http.MethodPost:
		l.unlock(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/locks/"), "/unlock"))
	default:
		lfsError(w, http.StatusNotFound, "not found")
	}
}

func (l *Local) listLocks(w http.ResponseWriter, r *http.Request) {
	locks, err := l.locks.load()
	if err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	var out []Lock
	for _, lk := range locks {
		if (q.Get("path") == "" || q.Get("path") == lk.Path) && (q.Get("id") == "" || q.Get("id") == lk.ID) &&
			(q.Get("refspec") == "" || lk.Ref == "" || q.Get("refspec") == lk.Ref) {
			out = append(out, lk)
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, next := pageLocks(out, q.Get("cursor"), limit)
	if page == nil {
		page = []Lock{}
	}
	resp := map[string]any{"locks": page}
	if next != "" {
		resp["next_cursor"] = next
	}
	lfsJSON(w, r, http.StatusOK, resp)
}

func (l *Local) createLock(w http.ResponseWriter, r *http.Request) {
	if !l.AllowPush {
		lfsError(w, http.StatusForbidden, "push disabled for this repository")
		return
	}
	var req struct {
		Path string   `json:"path"`
		Ref  *refSpec `json:"ref,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Path == "" {
		lfsError(w, http.StatusUnprocessableEntity, "invalid lock request")
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	locks, err := l.locks.load()
	if err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, lk := range locks {
		if lk.Path == req.Path {
			lfsJSON(w, r, http.StatusConflict, map[string]any{"lock": lk, "message": "already created lock"})
			return
		}
	}
	lk := Lock{ID: newLockID(), Path: req.Path, LockedAt: time.Now().UTC().Truncate(time.Second), Owner: lockOwner{Name: owner(r)}}
	if req.Ref != nil {
		lk.Ref = req.Ref.Name
	}
	if err := l.locks.save(append(locks, lk)); err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lfsJSON(w, r, http.StatusCreated, map[string]any{"lock": lk})
}

func (l *Local) verifyLocks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	locks, err := l.locks.load()
	if err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	page, next := pageLocks(locks, req.Cursor, req.Limit)
	me := owner(r)
	ours, theirs := []Lock{}, []Lock{}
	for _, lk := range page {
		if lk.Owner.Name == me {
			ours = append(ours, lk)
		} else {
			theirs = append(theirs, lk)
		}
	}
	resp := map[string]any{"ours": ours, "theirs": theirs}
	if next != "" {
		resp["next_cursor"] = next
	}
	lfsJSON(w, r, http.StatusOK, resp)
}

func (l *Local) unlock(w http.ResponseWriter, r *http.Request, id string) {
	if !l.AllowPush {
		lfsError(w, http.StatusForbidden, "push disabled for this repository")
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	l.mu.Lock()
	defer l.mu.Unlock()
	locks, err := l.locks.load()
	if err != nil {
		lfsError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i, lk := range locks {
		if lk.ID != id {
			continue
		}
		if lk.Owner.Name != owner(r) && !req.Force {
			lfsError(w, http.StatusForbidden, "lock is owned by "+lk.Owner.Name+"; use --force")
			return
		}
		if err := l.locks.save(append(locks[:i:i], locks[i+1:]...)); err != nil {
			lfsError(w, http.StatusInternalServerError, err.Error())
			return
		}
		lfsJSON(w, r, http.StatusOK, map[string]any{"lock": lk})
		return
	}
	lfsError(w, http.StatusNotFound, "lock not found")
}

// ── Optional interfaces ──

// ListPackages returns an empty list: LFS objects are content-addressed
// blobs, not named packages. Use Stats for usage.
func (l *Local) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return []registry.PackageSummary{}, nil
}

func (l *Local) Stats(context.Context) (registry.Stats, error) {
	return objectStats(l.store), nil
}

func objectStats(s *Store) registry.Stats {
	var st registry.Stats
	st.BlobCount, st.TotalBytes = s.Usage("objects")
	return st
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return objectDetail(l.store, name)
}

// objectDetail describes one object (name = oid).
func objectDetail(s *Store, oid string) (*registry.PackageDetail, error) {
	if !ValidOID(oid) {
		return nil, registry.ErrInvalidPackageName
	}
	fi, err := s.Stat(objectRel(oid))
	if err != nil {
		return nil, registry.ErrPackageNotFound
	}
	return &registry.PackageDetail{Type: typ, Name: oid, Generic: &registry.GenericPackageDetail{
		LatestVersion: oid,
		Versions: []registry.GenericVersionDetail{{
			Version: oid, Size: fi.Size, PublishedAt: fi.ModTime.UTC().Format(time.RFC3339),
			Files: []registry.GenericFile{{Name: oid, Size: fi.Size, SHA256: oid}},
		}},
	}}, nil
}

// DeleteVersion deletes the object name (= oid); version is ignored.
func (l *Local) DeleteVersion(_ context.Context, name, _ string) error {
	if _, ok := l.store.HasObject(name, -1); !ok {
		return registry.ErrPackageNotFound
	}
	if err := l.store.Delete(objectRel(name)); err != nil {
		return err
	}
	l.EmitDeleted(name)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	if !pkgbase.IsRead(r) {
		return registry.ArtifactRef{}, false
	}
	oid, ok := objectsOID(route(r.URL.Path))
	if !ok {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: oid, Version: oid}, true
}

// PromoteVersion copies object name (= oid) into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, _ string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("gitlfs: promote target %T is not a local gitlfs repository", dst)
	}
	size, ok := l.store.HasObject(name, -1)
	if !ok {
		return registry.ErrPackageNotFound
	}
	if _, exists := d.store.HasObject(name, size); exists {
		return nil
	}
	if code, err := d.Guard.Check(d.store.Store, false, size); code != 0 {
		return err
	}
	rc, _, err := l.store.Open(objectRel(name))
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := d.store.WriteStream(objectRel(name), rc, size); err != nil {
		return err
	}
	d.EmitPublished(name, size)
	return nil
}

// Objects lists every stored object id (sorted).
func (s *Store) Objects() ([]string, error) {
	var out []string
	err := s.Walk("objects", func(rel string, _ rawfs.DirEntry) error {
		if oid := rel[strings.LastIndex(rel, "/")+1:]; ValidOID(oid) {
			out = append(out, oid)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
