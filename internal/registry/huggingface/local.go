package huggingface

import (
	"context"
	"crypto/sha1" //nolint:gosec // git blob / commit ids are sha1
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// lfsThreshold mirrors the hub: files at or above it are reported as
// LFS files (X-Linked-Etag = sha256).
const lfsThreshold = 10 << 20

// Local layout (base-relative):
//
//	refs/{key}/{branch}           commit sha the branch points to
//	commits/{key}/{sha}.json      commit document (file list)
//	blobs/{sha256[0:2]}/{sha256}  content-addressed file bodies
type Local struct {
	*pkgbase.Repo
	store *Store
	mu    sync.Mutex
}

type commitDoc struct {
	SHA     string     `json:"sha"`
	Parent  string     `json:"parent,omitempty"`
	Branch  string     `json:"branch,omitempty"`
	Created time.Time  `json:"created"`
	Files   []fileMeta `json:"files"`
}

func (c *commitDoc) file(p string) (fileMeta, bool) {
	for _, f := range c.Files {
		if f.Path == p {
			return f, true
		}
	}
	return fileMeta{}, false
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		return &Local{Repo: base, store: &Store{base.Store}}, nil
	}
}

func (l *Local) Close() error  { return nil }
func (l *Local) Store() *Store { return l.store }

func blobRel(sha string) string { return path.Join("blobs", sha[:2], sha) }

func refRel(ref repoRef, branch string) string {
	return path.Join("refs", ref.key(), revKey(branch))
}

func commitRel(ref repoRef, sha string) string {
	return path.Join("commits", ref.key(), sha+".json")
}

func (s *Store) readCommit(ref repoRef, sha string) (*commitDoc, error) {
	b, err := s.Read(commitRel(ref, sha))
	if err != nil {
		return nil, registry.ErrPackageNotFound
	}
	var c commitDoc
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolve maps rev ("" = main, branch or sha) to a commit.
func (s *Store) resolve(ref repoRef, rev string) (*commitDoc, error) {
	if rev == "" {
		rev = "main"
	}
	if isSHA(rev) {
		if c, err := s.readCommit(ref, rev); err == nil {
			return c, nil
		}
	}
	b, err := s.Read(refRel(ref, rev))
	if err != nil {
		return nil, registry.ErrPackageNotFound
	}
	return s.readCommit(ref, strings.TrimSpace(string(b)))
}

// commits returns every commit of ref, newest first.
func (s *Store) commits(ref repoRef) []*commitDoc {
	files, _ := s.ListFiles(path.Join("commits", ref.key()))
	var out []*commitDoc
	for _, f := range files {
		if c, err := s.readCommit(ref, strings.TrimSuffix(f.Name, ".json")); err == nil {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (s *Store) repos() []repoRef {
	keys, _ := s.ListDirs("commits")
	var out []repoRef
	for _, k := range keys {
		if ref, ok := parseKey(k); ok {
			out = append(out, ref)
		}
	}
	return out
}

func newCommitSHA(parent string, created time.Time, files []fileMeta) string {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "parent %s\ntime %d\n", parent, created.UnixNano())
	for _, f := range files {
		fmt.Fprintf(h, "%s %s %d\n", f.Path, f.etag(), f.Size)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// commit records files on branch (replacing the previous head).
func (s *Store) commit(ref repoRef, branch string, files []fileMeta) (*commitDoc, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	c := &commitDoc{Branch: branch, Created: time.Now().UTC(), Files: files}
	if head, err := s.resolve(ref, branch); err == nil {
		c.Parent = head.SHA
	}
	c.SHA = newCommitSHA(c.Parent, c.Created, files)
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if err := s.Write(commitRel(ref, c.SHA), body); err != nil {
		return nil, err
	}
	if err := s.Write(refRel(ref, branch), []byte(c.SHA)); err != nil {
		return nil, err
	}
	return c, nil
}

// gcBlobs removes blobs no longer referenced by any commit.
func (s *Store) gcBlobs() {
	used := map[string]bool{}
	for _, ref := range s.repos() {
		for _, c := range s.commits(ref) {
			for _, f := range c.Files {
				used[f.SHA256] = true
			}
		}
	}
	dirs, _ := s.ListDirs("blobs")
	for _, d := range dirs {
		files, _ := s.ListFiles(path.Join("blobs", d))
		for _, f := range files {
			if !used[f.Name] {
				_ = s.Delete(path.Join("blobs", d, f.Name))
			}
		}
	}
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, ok := parseRoute(r.URL.EscapedPath())
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveLocalRead(w, r, l.store, rt)
	case http.MethodPut, http.MethodPost:
		if rt.op != opResolve {
			pkgbase.Error(w, http.StatusNotImplemented, "only raw PUT /{repo}/resolve/{branch}/{path} uploads are supported")
			return
		}
		l.upload(w, r, rt)
	case http.MethodDelete:
		if rt.op != opResolve {
			pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		l.removeFile(w, r, rt)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func repoInfo(ref repoRef, c *commitDoc) map[string]any {
	sib := make([]map[string]any, 0, len(c.Files))
	for _, f := range c.Files {
		s := map[string]any{"rfilename": f.Path, "size": f.Size, "blobId": f.BlobID}
		if f.LFS {
			s["blobId"] = gitBlobID([]byte(lfsPointer(f.SHA256, f.Size)))
			s["lfs"] = map[string]any{"sha256": f.SHA256, "size": f.Size, "pointerSize": len(lfsPointer(f.SHA256, f.Size))}
		}
		sib = append(sib, s)
	}
	return map[string]any{
		"_id": c.SHA, "id": ref.ID, "modelId": ref.ID, "author": strings.SplitN(ref.ID, "/", 2)[0],
		"sha": c.SHA, "lastModified": c.Created.Format(time.RFC3339), "createdAt": c.Created.Format(time.RFC3339),
		"private": false, "disabled": false, "gated": false, "downloads": 0, "likes": 0,
		"tags": []string{}, "siblings": sib,
	}
}

func treeEntries(c *commitDoc, dir string, recursive bool) []map[string]any {
	dir = strings.Trim(dir, "/")
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	var out []map[string]any
	seenDir := map[string]bool{}
	for _, f := range c.Files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(f.Path, prefix)
		parts := strings.Split(rest, "/")
		for i := 1; i < len(parts); i++ {
			if !recursive && i > 1 {
				break
			}
			d := prefix + strings.Join(parts[:i], "/")
			if !seenDir[d] {
				seenDir[d] = true
				out = append(out, map[string]any{"type": "directory", "oid": "", "size": 0, "path": d})
			}
		}
		if len(parts) > 1 && !recursive {
			continue
		}
		e := map[string]any{"type": "file", "oid": f.BlobID, "size": f.Size, "path": f.Path}
		if f.LFS {
			ptr := lfsPointer(f.SHA256, f.Size)
			e["oid"] = gitBlobID([]byte(ptr))
			e["lfs"] = map[string]any{"oid": f.SHA256, "size": f.Size, "pointerSize": len(ptr)}
		}
		out = append(out, e)
	}
	return out
}

func serveLocalRead(w http.ResponseWriter, r *http.Request, s *Store, rt route) {
	switch rt.op {
	case opWhoami:
		whoami(w, r)
	case opInfo, opTree, opResolve:
		c, err := s.resolve(rt.ref, rt.rev)
		if err != nil {
			code := "RevisionNotFound"
			if len(s.commits(rt.ref)) == 0 {
				code = "RepoNotFound"
			}
			hfError(w, http.StatusNotFound, code, "repository or revision not found")
			return
		}
		switch rt.op {
		case opInfo:
			pkgbase.WriteJSON(w, r, http.StatusOK, repoInfo(rt.ref, c))
		case opTree:
			entries := treeEntries(c, rt.file, r.URL.Query().Get("recursive") == "true" || r.URL.Query().Get("recursive") == "1")
			if len(entries) == 0 && rt.file != "" {
				hfError(w, http.StatusNotFound, "EntryNotFound", "entry not found")
				return
			}
			if entries == nil {
				entries = []map[string]any{}
			}
			pkgbase.WriteJSON(w, r, http.StatusOK, entries)
		default:
			f, ok := c.file(rt.file)
			if !ok {
				hfError(w, http.StatusNotFound, "EntryNotFound", "entry not found")
				return
			}
			setFileHeaders(w, c.SHA, f)
			if !pkgbase.ServeStored(w, r, s.Store, blobRel(f.SHA256), "application/octet-stream") {
				w.Header().Del("ETag")
				hfError(w, http.StatusNotFound, "EntryNotFound", "file content missing")
			}
		}
	default:
		pkgbase.NotFound(w)
	}
}

// spool copies body into a temp file (bounded by max) and returns the
// file, size, sha256 and git blob id.
func spool(body io.Reader, max int64) (*os.File, fileMeta, error) {
	if max <= 0 {
		max = pkgbase.DefaultMaxUpload
	}
	tmp, err := os.CreateTemp("", "kutu-hf-up-*")
	if err != nil {
		return nil, fileMeta{}, err
	}
	fail := func(err error) (*os.File, fileMeta, error) {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, fileMeta{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, max+1))
	if err != nil {
		return fail(err)
	}
	if n > max {
		return fail(fmt.Errorf("upload exceeds %d bytes", max))
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	b1 := sha1.New() //nolint:gosec
	fmt.Fprintf(b1, "blob %d\x00", n)
	if err := hashFile(tmp, b1); err != nil {
		return fail(err)
	}
	return tmp, fileMeta{Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), BlobID: hex.EncodeToString(b1.Sum(nil)), LFS: n >= lfsThreshold}, nil
}

func hashFile(f *os.File, h hash.Hash) error {
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request, rt route) {
	if !l.CheckPush(w) {
		return
	}
	if isSHA(rt.rev) {
		pkgbase.Error(w, http.StatusBadRequest, "uploads must target a branch, not a commit")
		return
	}
	tmp, meta, err := spool(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	meta.Path = rt.file

	l.mu.Lock()
	defer l.mu.Unlock()
	var files []fileMeta
	exists := false
	if head, err := l.store.resolve(rt.ref, rt.rev); err == nil {
		for _, f := range head.Files {
			if f.Path == rt.file {
				exists = true
				continue
			}
			files = append(files, f)
		}
	}
	if !l.AllowPublish(w, exists, meta.Size) {
		return
	}
	if !l.store.Exists(blobRel(meta.SHA256)) {
		if err := l.store.WriteStream(blobRel(meta.SHA256), tmp, meta.Size); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	c, err := l.store.commit(rt.ref, rt.rev, append(files, meta))
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(rt.ref.name()+"@"+c.SHA+"/"+rt.file, meta.Size)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"commit": c.SHA, "branch": rt.rev, "path": rt.file, "size": meta.Size, "sha256": meta.SHA256,
	})
}

func (l *Local) removeFile(w http.ResponseWriter, r *http.Request, rt route) {
	if !l.CheckPush(w) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	head, err := l.store.resolve(rt.ref, rt.rev)
	if err != nil || isSHA(rt.rev) {
		pkgbase.NotFound(w)
		return
	}
	if _, ok := head.file(rt.file); !ok {
		pkgbase.NotFound(w)
		return
	}
	var files []fileMeta
	for _, f := range head.Files {
		if f.Path != rt.file {
			files = append(files, f)
		}
	}
	c, err := l.store.commit(rt.ref, rt.rev, files)
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitDeleted(rt.ref.name() + "/" + rt.file)
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"commit": c.SHA})
}

// ── Local optional interfaces ──

func (l *Local) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	var out []registry.PackageSummary
	for _, ref := range l.store.repos() {
		cs := l.store.commits(ref)
		if len(cs) == 0 {
			continue
		}
		vs := make([]string, len(cs))
		for i, c := range cs {
			vs[len(cs)-1-i] = c.SHA
		}
		out = append(out, registry.PackageSummary{Name: ref.name(), Versions: vs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (l *Local) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, l, l.store.Store), nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	ref, ok := parseName(name)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	cs := l.store.commits(ref)
	if len(cs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d := &registry.GenericPackageDetail{Metadata: map[string]string{"repo_type": strings.TrimSuffix(ref.Kind, "s")}}
	if head, err := l.store.resolve(ref, "main"); err == nil {
		d.LatestVersion = head.SHA
		d.License = l.license(head)
	} else {
		d.LatestVersion = cs[0].SHA
	}
	for _, c := range cs {
		row := registry.GenericVersionDetail{Version: c.SHA, PublishedAt: c.Created.Format(time.RFC3339)}
		if c.Branch != "" || c.Parent != "" {
			row.Metadata = map[string]string{}
			if c.Branch != "" {
				row.Metadata["branch"] = c.Branch
			}
			if c.Parent != "" {
				row.Metadata["parent"] = c.Parent
			}
		}
		for _, f := range c.Files {
			row.Size += f.Size
			row.Files = append(row.Files, registry.GenericFile{Name: f.Path, Size: f.Size, SHA256: f.SHA256})
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: ref.name(), Generic: d}, nil
}

func (l *Local) license(c *commitDoc) string {
	f, ok := c.file("README.md")
	if !ok || f.Size > 1<<20 {
		return ""
	}
	b, err := l.store.Read(blobRel(f.SHA256))
	if err != nil {
		return ""
	}
	return readmeLicense(b)
}

// DeleteVersion removes one commit. Branches pointing at it move to
// its parent (or are deleted); unreferenced blobs are collected.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	ref, ok := parseName(name)
	if !ok {
		return registry.ErrPackageNotFound
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c, err := l.store.readCommit(ref, version)
	if err != nil {
		return registry.ErrPackageNotFound
	}
	if err := l.store.Delete(commitRel(ref, c.SHA)); err != nil {
		return err
	}
	parent := ""
	if c.Parent != "" && l.store.Exists(commitRel(ref, c.Parent)) {
		parent = c.Parent
	}
	branches, _ := l.store.ListFiles(path.Join("refs", ref.key()))
	for _, b := range branches {
		rel := path.Join("refs", ref.key(), b.Name)
		head, err := l.store.Read(rel)
		if err != nil || strings.TrimSpace(string(head)) != c.SHA {
			continue
		}
		if parent != "" {
			err = l.store.Write(rel, []byte(parent))
		} else {
			err = l.store.Delete(rel)
		}
		if err != nil {
			return err
		}
	}
	l.store.gcBlobs()
	l.EmitDeleted(ref.name() + "@" + c.SHA)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	rp, ok := parseName(ref.Name)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	c, err := l.store.resolve(rp, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	return registry.ArtifactMeta{License: l.license(c), PublishedAt: c.Created}, nil
}

// PromoteVersion copies commit `version` (or a branch name) of name
// into dst and points dst's branch (the commit's branch, default
// "main") at it.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("huggingface: promote target %T is not a local huggingface repository", dst)
	}
	ref, ok := parseName(name)
	if !ok {
		return registry.ErrPackageNotFound
	}
	c, err := l.store.resolve(ref, version)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range c.Files {
		total += f.Size
	}
	if code, err := d.Guard.Check(d.store.Store, d.store.Exists(commitRel(ref, c.SHA)), total); code != 0 {
		return err
	}
	for _, f := range c.Files {
		if d.store.Exists(blobRel(f.SHA256)) {
			continue
		}
		rc, fi, err := l.store.Open(blobRel(f.SHA256))
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		size := f.Size
		if fi != nil {
			size = fi.Size
		}
		err = d.store.WriteStream(blobRel(f.SHA256), rc, size)
		rc.Close()
		if err != nil {
			return err
		}
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.store.Write(commitRel(ref, c.SHA), body); err != nil {
		return err
	}
	branch := c.Branch
	if branch == "" {
		branch = "main"
	}
	if err := d.store.Write(refRel(ref, branch), []byte(c.SHA)); err != nil {
		return err
	}
	d.EmitPublished(ref.name()+"@"+c.SHA, total)
	return nil
}
