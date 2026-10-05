package huggingface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/service"
)

// fileTimeout bounds one upstream file transfer (model shards can be
// tens of GB).
const fileTimeout = 12 * time.Hour

// Remote layout (base-relative):
//
//	refs/{key}/{rev}.json        repo info at a branch/tag (MutableTTL)
//	commits/{key}/{sha}.json     repo info at a commit (?blobs=true, forever)
//	files/{key}/{sha}/{path}     file content (forever)
//	tree/…, api/…                proxied listings (MutableTTL)
type Remote struct {
	*pkgbase.RemoteRepo
	store  *Store
	files  *upstream.Client
	flight inflight
}

// Store wraps pkgbase.Store with the hub layout.
type Store struct{ *pkgbase.Store }

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/api/models?limit=1")
		if err != nil {
			return nil, err
		}
		files, err := upstream.NewClient(upstream.Config{
			BaseURL: r.URL, Auth: r.Auth, Resolver: deps.Resolver,
			InsecureSkipVerify: r.InsecureSkipVerify, Timeout: fileTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("%s/remote %s/%s: %w", typ, ns, r.Name, err)
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}, files: files}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func (rr *Remote) Close() error {
	_ = rr.files.Close()
	return rr.RemoteRepo.Close()
}

// upstreamInfo is the subset of the hub repo-info document kutu reads.
type upstreamInfo struct {
	ID           string `json:"id"`
	SHA          string `json:"sha"`
	LastModified string `json:"lastModified,omitempty"`
	CardData     *struct {
		License any `json:"license,omitempty"`
	} `json:"cardData,omitempty"`
	Siblings []sibling `json:"siblings"`
}

type sibling struct {
	RFilename string   `json:"rfilename"`
	Size      int64    `json:"size,omitempty"`
	BlobID    string   `json:"blobId,omitempty"`
	LFS       *lfsInfo `json:"lfs,omitempty"`
}

type lfsInfo struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	PointerSize int    `json:"pointerSize,omitempty"`
}

func (u *upstreamInfo) file(p string) (fileMeta, bool) {
	for _, s := range u.Siblings {
		if s.RFilename != p {
			continue
		}
		f := fileMeta{Path: p, Size: s.Size, BlobID: s.BlobID}
		if s.LFS != nil && s.LFS.SHA256 != "" {
			f.SHA256, f.LFS = s.LFS.SHA256, true
			if s.LFS.Size > 0 {
				f.Size = s.LFS.Size
			}
		}
		return f, true
	}
	return fileMeta{}, false
}

func (u *upstreamInfo) license() string {
	if u.CardData == nil {
		return ""
	}
	switch v := u.CardData.License.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " OR ")
	}
	return ""
}

func revKey(rev string) string {
	if rev == "" {
		return "_default"
	}
	return url.PathEscape(rev)
}

func (rr *Remote) infoPath(ref repoRef, rev string) string {
	p := ref.apiPath()
	if rev != "" {
		p += "/revision/" + url.PathEscape(rev)
	}
	return p + "?blobs=true"
}

// resolveCommit resolves rev ("" = default branch) to a commit and its
// repo-info document.
func (rr *Remote) resolveCommit(ctx context.Context, ref repoRef, rev string) (*upstreamInfo, []byte, error) {
	cacheRel := path.Join("refs", ref.key(), revKey(rev)+".json")
	mutable := true
	if isSHA(rev) {
		cacheRel, mutable = path.Join("commits", ref.key(), rev+".json"), false
	}
	body, err := rr.FetchCached(ctx, cacheRel, rr.infoPath(ref, rev), mutable)
	if err != nil {
		return nil, nil, err
	}
	var info upstreamInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, nil, fmt.Errorf("decode repo info: %w", err)
	}
	if !isSHA(info.SHA) {
		return nil, nil, fmt.Errorf("upstream repo info has no commit sha")
	}
	if mutable {
		if crel := path.Join("commits", ref.key(), info.SHA+".json"); !rr.store.Exists(crel) {
			_ = rr.store.Write(crel, body)
		}
	}
	return &info, body, nil
}

func resolveError(w http.ResponseWriter, rev string, err error) {
	if errors.Is(err, upstream.ErrNotFound) {
		if rev == "" || rev == "main" {
			hfError(w, http.StatusNotFound, "RepoNotFound", "repository not found")
		} else {
			hfError(w, http.StatusNotFound, "RevisionNotFound", "revision not found")
		}
		return
	}
	pkgbase.UpstreamError(w, err)
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	rt, ok := parseRoute(r.URL.EscapedPath())
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	switch rt.op {
	case opWhoami:
		whoami(w, r)
	case opInfo:
		_, body, err := rr.resolveCommit(r.Context(), rt.ref, rt.rev)
		if err != nil {
			resolveError(w, rt.rev, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", body)
	case opTree:
		rr.proxyAPI(w, r, path.Join("tree", rt.ref.key(), revKey(rt.rev), rt.file))
	case opResolve:
		rr.serveFile(w, r, rt)
	default:
		rr.proxyAPI(w, r, path.Join("api", strings.Trim(r.URL.EscapedPath(), "/")))
	}
}

// proxyAPI proxies an API GET (query included) with MutableTTL caching.
func (rr *Remote) proxyAPI(w http.ResponseWriter, r *http.Request, dir string) {
	q := sha256.Sum256([]byte(r.URL.RawQuery))
	cacheRel := path.Join(dir, "_q"+hex.EncodeToString(q[:8])+".json")
	up := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		up += "?" + r.URL.RawQuery
	}
	rr.ServeCached(w, r, cacheRel, up, "application/json", true)
}

func (rr *Remote) serveFile(w http.ResponseWriter, r *http.Request, rt route) {
	info, _, err := rr.resolveCommit(r.Context(), rt.ref, rt.rev)
	if err != nil {
		resolveError(w, rt.rev, err)
		return
	}
	f, ok := info.file(rt.file)
	if !ok {
		hfError(w, http.StatusNotFound, "EntryNotFound", "entry not found")
		return
	}
	setFileHeaders(w, info.SHA, f)
	rel := filesRel(rt.ref, info.SHA, f.Path)
	if !rr.flight.busy(rel) && pkgbase.ServeStored(w, r, rr.store.Store, rel, "application/octet-stream") {
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/octet-stream")
		if f.Size > 0 || f.BlobID != "" || f.LFS {
			w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("Range") != "" || rr.flight.busy(rel) {
		rr.passThrough(w, r, rt.ref, info.SHA, f)
		return
	}
	err = rr.fill(context.WithoutCancel(r.Context()), rt.ref, info.SHA, f, func(size int64) io.Writer {
		w.Header().Set("Content-Type", "application/octet-stream")
		if size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.WriteHeader(http.StatusOK)
		return w
	})
	if err != nil && !errors.Is(err, errStarted) {
		if errors.Is(err, errBusy) {
			rr.passThrough(w, r, rt.ref, info.SHA, f)
			return
		}
		pkgbase.UpstreamError(w, err)
	}
}

var (
	errBusy    = errors.New("file is being cached by another request")
	errStarted = errors.New("response already started")
)

func (rr *Remote) fileURL(ref repoRef, commit, file string) string {
	return ref.webPrefix() + "/resolve/" + commit + "/" + escFile(file)
}

// passThrough streams the upstream response without caching (Range
// requests and concurrent fills).
func (rr *Remote) passThrough(w http.ResponseWriter, r *http.Request, ref repoRef, commit string, f fileMeta) {
	hdr := http.Header{}
	if v := r.Header.Get("Range"); v != "" {
		hdr.Set("Range", v)
	}
	resp, err := rr.files.GetWithHeaders(r.Context(), rr.fileURL(ref, commit, f.Path), hdr)
	if err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	for _, h := range []string{"Content-Length", "Content-Range"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// fill downloads one file into the cache, verifying size and hash.
// start (optional) is called once the upstream answered and returns
// a writer receiving a copy of the stream.
func (rr *Remote) fill(ctx context.Context, ref repoRef, commit string, f fileMeta, start func(size int64) io.Writer) error {
	rel := filesRel(ref, commit, f.Path)
	if !rr.flight.acquire(rel) {
		return errBusy
	}
	defer rr.flight.release(rel)
	resp, err := rr.files.Get(ctx, rr.fileURL(ref, commit, f.Path))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	known := f.LFS || f.BlobID != ""
	size := f.Size
	if !known {
		size = resp.ContentLength
		if size >= 0 {
			f.Size = size
		}
	}
	ver := newVerifier(f)
	writers := []io.Writer{ver}
	started := false
	if start != nil {
		writers = append(writers, &lenientWriter{w: start(size)})
		started = true
	}
	src := io.TeeReader(resp.Body, io.MultiWriter(writers...))
	if size >= 0 {
		err = rr.store.WriteStream(rel, src, size)
	} else {
		err = rr.writeViaTemp(rel, src)
	}
	if err == nil && (known || size >= 0) {
		err = ver.check()
	}
	if err != nil {
		_ = rr.store.Delete(rel)
		if started {
			return fmt.Errorf("%w: %w", errStarted, err)
		}
		return err
	}
	return nil
}

// writeViaTemp spools a stream of unknown length to a temp file so the
// store receives an exact size.
func (rr *Remote) writeViaTemp(rel string, src io.Reader) error {
	tmp, err := os.CreateTemp("", "kutu-hf-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, src)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return rr.store.WriteStream(rel, tmp, n)
}

// ── Remote optional interfaces ──

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return listCached(rr.store)
}

func listCached(s *Store) ([]registry.PackageSummary, error) {
	keys, err := s.ListDirs("files")
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, k := range keys {
		ref, ok := parseKey(k)
		if !ok {
			continue
		}
		shas := cachedCommits(s, ref)
		if len(shas) == 0 {
			continue
		}
		vs := make([]string, len(shas))
		for i, c := range shas {
			vs[len(shas)-1-i] = c.sha
		}
		out = append(out, registry.PackageSummary{Name: ref.name(), Versions: vs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type cachedCommit struct {
	sha   string
	mod   time.Time
	files []registry.GenericFile
	size  int64
}

// cachedCommits returns the cached commits of ref, newest first.
func cachedCommits(s *Store, ref repoRef) []cachedCommit {
	dirs, _ := s.ListDirs(path.Join("files", ref.key()))
	var out []cachedCommit
	for _, d := range dirs {
		c := cachedCommit{sha: d}
		base := path.Join("files", ref.key(), d)
		_ = s.Walk(base, func(rel string, e rawfs.DirEntry) error {
			c.files = append(c.files, registry.GenericFile{Name: strings.TrimPrefix(rel, base+"/"), Size: e.Size})
			c.size += e.Size
			if fi, err := s.Stat(rel); err == nil && fi.ModTime.After(c.mod) {
				c.mod = fi.ModTime
			}
			return nil
		})
		if len(c.files) > 0 {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	return out
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	ref, ok := parseName(name)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	commits := cachedCommits(rr.store, ref)
	if len(commits) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d := &registry.GenericPackageDetail{
		LatestVersion: commits[0].sha,
		Homepage:      rr.Client.BaseURL() + ref.webPrefix(),
		Metadata:      map[string]string{"repo_type": strings.TrimSuffix(ref.Kind, "s")},
	}
	for _, c := range commits {
		row := registry.GenericVersionDetail{Version: c.sha, Size: c.size, Files: c.files}
		if !c.mod.IsZero() {
			row.PublishedAt = c.mod.UTC().Format(time.RFC3339)
		}
		if b, err := rr.store.Read(path.Join("commits", ref.key(), c.sha+".json")); err == nil {
			var info upstreamInfo
			if json.Unmarshal(b, &info) == nil {
				if l := info.license(); l != "" && d.License == "" {
					d.License = l
				}
				if info.LastModified != "" {
					row.Metadata = map[string]string{"last_modified": info.LastModified}
				}
			}
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: ref.name(), Generic: d}, nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, "refs", "tree", "api"), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	rp, ok := parseName(ref.Name)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	info, _, err := rr.resolveCommit(ctx, rp, ref.Version)
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return registry.ArtifactMeta{}, registry.ErrPackageNotFound
		}
		return registry.ArtifactMeta{}, err
	}
	m := registry.ArtifactMeta{License: info.license()}
	if t, err := time.Parse(time.RFC3339, info.LastModified); err == nil {
		m.PublishedAt = t
	}
	return m, nil
}

// Prefetch warms the repo info and every file of the revision
// (version "" = default branch).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	ref, ok := parseName(name)
	if !ok {
		return registry.ErrInvalidPackageName
	}
	info, _, err := rr.resolveCommit(ctx, ref, version)
	if err != nil {
		return err
	}
	for _, s := range info.Siblings {
		f, _ := info.file(s.RFilename)
		if !validFile(f.Path) || rr.store.Exists(filesRel(ref, info.SHA, f.Path)) {
			continue
		}
		if err := rr.fill(ctx, ref, info.SHA, f, nil); err != nil && !errors.Is(err, errBusy) {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	return nil
}
