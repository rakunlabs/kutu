// Package pkgbase holds the plumbing shared by the path-keyed registry
// protocols (generic, nuget, rubygems, apt, …): a rawfs-backed file
// store rooted at a repo base path, small HTTP helpers, a pull-through
// remote cache helper and a Local/Remote identity mixin.
//
// Each protocol package keeps its own wire format and index
// generation; pkgbase only removes the boilerplate every one of them
// would otherwise duplicate.
package pkgbase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/events"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/service"
)

// DefaultMaxUpload is the upload cap used when a repo leaves
// MaxUploadSize at zero.
const DefaultMaxUpload int64 = 1 << 30

// ErrReadOnly is returned when the backing mount cannot be written.
var ErrReadOnly = errors.New("registry backend is read-only")

// ── Store ──

// Store is a rawfs-backed, path-keyed file store rooted at a base path.
type Store struct {
	fs       rawfs.RawFS
	basePath string
}

// NewStore returns a store rooted at basePath on fs.
func NewStore(fs rawfs.RawFS, basePath string) *Store {
	return &Store{fs: fs, basePath: strings.Trim(basePath, "/")}
}

// RawFS exposes the underlying filesystem.
func (s *Store) RawFS() rawfs.RawFS { return s.fs }

// Join builds an absolute store path from relative segments. Segments
// are cleaned so ".." can never escape the base path.
func (s *Store) Join(parts ...string) string {
	cleaned := make([]string, 0, len(parts)+1)
	if s.basePath != "" {
		cleaned = append(cleaned, s.basePath)
	}
	for _, p := range parts {
		p = strings.Trim(path.Clean("/"+p), "/")
		if p != "" && p != "." {
			cleaned = append(cleaned, p)
		}
	}
	return path.Join(cleaned...)
}

// Rel converts an absolute store path back to a base-relative path.
func (s *Store) Rel(abs string) string {
	if s.basePath == "" {
		return strings.TrimPrefix(abs, "/")
	}
	return strings.TrimPrefix(strings.TrimPrefix(abs, s.basePath), "/")
}

// Writable reports whether the backend accepts writes.
func (s *Store) Writable() bool { return rawfs.IsWritable(s.fs) }

// Write stores body at rel.
func (s *Store) Write(rel string, body []byte) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return ErrReadOnly
	}
	return wfs.Write(s.Join(rel), bytes.NewReader(body), int64(len(body)))
}

// WriteStream stores size bytes from r at rel.
func (s *Store) WriteStream(rel string, r io.Reader, size int64) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return ErrReadOnly
	}
	return wfs.Write(s.Join(rel), r, size)
}

// Read returns the full contents at rel.
func (s *Store) Read(rel string) ([]byte, error) {
	rc, _, err := s.fs.Open(s.Join(rel))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Open opens rel for streaming.
func (s *Store) Open(rel string) (rawfs.ReadSeekCloser, *rawfs.FileInfo, error) {
	return s.fs.Open(s.Join(rel))
}

// Stat stats rel.
func (s *Store) Stat(rel string) (*rawfs.FileInfo, error) { return s.fs.Stat(s.Join(rel)) }

// Exists reports whether rel exists.
func (s *Store) Exists(rel string) bool {
	_, err := s.fs.Stat(s.Join(rel))
	return err == nil
}

// Delete removes rel. Missing files are not an error.
func (s *Store) Delete(rel string) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return ErrReadOnly
	}
	if err := wfs.Delete(s.Join(rel)); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// ReadDir lists rel. A missing directory yields an empty list.
func (s *Store) ReadDir(rel string) ([]rawfs.DirEntry, error) {
	entries, err := s.fs.ReadDir(s.Join(rel))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// ListDirs returns the sorted names of sub-directories of rel.
func (s *Store) ListDirs(rel string) ([]string, error) {
	entries, err := s.ReadDir(rel)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListFiles returns the sorted names of regular files directly in rel.
func (s *Store) ListFiles(rel string) ([]rawfs.DirEntry, error) {
	entries, err := s.ReadDir(rel)
	if err != nil {
		return nil, err
	}
	var out []rawfs.DirEntry
	for _, e := range entries {
		if !e.IsDir {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// WalkFunc receives base-relative paths of regular files.
type WalkFunc func(rel string, e rawfs.DirEntry) error

// Walk visits every regular file under rel (depth first, sorted).
func (s *Store) Walk(rel string, fn WalkFunc) error {
	entries, err := s.ReadDir(rel)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for _, e := range entries {
		child := path.Join(rel, e.Name)
		if e.IsDir {
			if err := s.Walk(child, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(child, e); err != nil {
			return err
		}
	}
	return nil
}

// DeleteTree removes every file under rel and returns the count and
// bytes removed.
func (s *Store) DeleteTree(rel string) (int, int64, []error) {
	var count int
	var size int64
	var errs []error
	err := s.Walk(rel, func(child string, e rawfs.DirEntry) error {
		if err := s.Delete(child); err != nil {
			errs = append(errs, err)
			return nil
		}
		count++
		size += e.Size
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return count, size, errs
}

// Usage returns file count and total bytes under rel.
func (s *Store) Usage(rel string) (int, int64) {
	var count int
	var size int64
	_ = s.Walk(rel, func(_ string, e rawfs.DirEntry) error {
		count++
		size += e.Size
		return nil
	})
	return count, size
}

// ── HTTP helpers ──

// Prefix returns the URL prefix the registry is mounted under
// ("/registries/{ns}/{repo}" or "" on a dedicated listener).
func Prefix(r *http.Request) string {
	return strings.TrimRight(r.Header.Get("X-Pika-Registry-Prefix"), "/")
}

// PublicBase reconstructs the absolute client-facing base URL of the
// registry (scheme://host/prefix), honouring X-Forwarded-* headers.
func PublicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	u := &url.URL{Scheme: scheme, Host: host, Path: Prefix(r)}
	return strings.TrimRight(u.String(), "/")
}

// IsRead reports GET/HEAD.
func IsRead(r *http.Request) bool { return r.Method == http.MethodGet || r.Method == http.MethodHead }

// Error writes a JSON error body.
func Error(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}

// NotFound writes a 404 JSON error.
func NotFound(w http.ResponseWriter) { Error(w, http.StatusNotFound, "not found") }

// WriteBytes writes body with contentType, honouring HEAD.
func WriteBytes(w http.ResponseWriter, r *http.Request, code int, contentType string, body []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// WriteJSON marshals v and writes it.
func WriteJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	body, err := marshalJSON(v)
	if err != nil {
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteBytes(w, r, code, "application/json", body)
}

// ServeStored streams a stored file (with Range support).
func ServeStored(w http.ResponseWriter, r *http.Request, s *Store, rel, contentType string) bool {
	rc, fi, err := s.Open(rel)
	if err != nil {
		return false
	}
	defer rc.Close()
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	var mod time.Time
	if fi != nil {
		mod = fi.ModTime
	}
	http.ServeContent(w, r, path.Base(rel), mod, rc)
	return true
}

// ReadBody reads at most max bytes (0 = DefaultMaxUpload).
func ReadBody(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxUpload
	}
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("upload exceeds %d bytes", max)
	}
	return body, nil
}

// SHA256Hex returns the hex sha256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// IsNotFound matches rawfs/upstream not-found errors.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, service.ErrNotFound) || errors.Is(err, upstream.ErrNotFound) || errors.Is(err, registry.ErrPackageNotFound) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "not found") || strings.Contains(low, "no such file") || strings.Contains(low, "does not exist")
}

// UpstreamError maps an upstream error to an HTTP response.
func UpstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		NotFound(w)
	case errors.Is(err, upstream.ErrUnauthorized):
		Error(w, http.StatusBadGateway, "upstream unauthorized")
	default:
		Error(w, http.StatusBadGateway, "upstream: "+err.Error())
	}
}

// ── Identity mixins ──

// Repo carries the identity + common settings of one repository row.
type Repo struct {
	NS        string
	RepoName  string
	RepoType  string
	RepoKind  string
	AllowPush bool
	MaxUpload int64
	Emitter   events.Emitter
	Store     *Store
	Guard     PublishGuard
}

func (b *Repo) Namespace() string { return b.NS }
func (b *Repo) Name() string      { return b.RepoName }
func (b *Repo) Type() string      { return b.RepoType }
func (b *Repo) Kind() string      { return b.RepoKind }

// EmitPublished fires registry.published.
func (b *Repo) EmitPublished(subject string, size int64) {
	events.EmitSafe(b.Emitter, hook.Event{Type: hook.EventRegistryPublished, Mount: b.NS, Path: b.RepoName + "/" + subject, Protocol: "registry-" + b.RepoType, Size: size})
}

// EmitDeleted fires registry.deleted.
func (b *Repo) EmitDeleted(subject string) {
	events.EmitSafe(b.Emitter, hook.Event{Type: hook.EventRegistryDeleted, Mount: b.NS, Path: b.RepoName + "/" + subject, Protocol: "registry-" + b.RepoType})
}

// CheckPush writes 405 and returns false when pushes are disabled.
func (b *Repo) CheckPush(w http.ResponseWriter) bool {
	if !b.AllowPush {
		Error(w, http.StatusMethodNotAllowed, "push disabled for this repository")
		return false
	}
	return true
}

// AllowPublish applies the immutable-version and quota policies.
// exists reports whether the target version is already stored.
func (b *Repo) AllowPublish(w http.ResponseWriter, exists bool, incoming int64) bool {
	return b.Guard.Allow(w, b.Store, exists, incoming)
}

// NewLocalRepo builds a Repo for a local-kind row.
func NewLocalRepo(deps registry.Deps, typ, ns string, r *service.RegistryRepository) (*Repo, error) {
	fs, err := deps.MountRawFS(r.Mount)
	if err != nil {
		return nil, fmt.Errorf("%s/local %s/%s: %w", typ, ns, r.Name, err)
	}
	return &Repo{
		NS: ns, RepoName: r.Name, RepoType: typ, RepoKind: service.RegistryKindLocal,
		AllowPush: r.AllowPush, MaxUpload: r.MaxUploadSize, Emitter: deps.Emitter,
		Store: NewStore(fs, r.BasePath), Guard: GuardFor(r),
	}, nil
}

// RemoteRepo is the shared state of a pull-through remote repo.
type RemoteRepo struct {
	Repo
	Client     *upstream.Client
	Router     *upstream.Router
	MutableTTL time.Duration
	ProbePath  string
}

// NewRemoteRepo builds a RemoteRepo for a remote-kind row.
func NewRemoteRepo(deps registry.Deps, typ, ns string, r *service.RegistryRepository, probePath string) (*RemoteRepo, error) {
	b, err := upstream.BuildRemote(deps, typ+"/remote", ns, r, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	return &RemoteRepo{
		Repo: Repo{
			NS: ns, RepoName: r.Name, RepoType: typ, RepoKind: service.RegistryKindRemote,
			MaxUpload: r.MaxUploadSize, Emitter: deps.Emitter, Store: NewStore(b.FS, b.BasePath),
		},
		Client:     b.Client,
		Router:     upstream.NewRouter(b.Client, b.Upstreams),
		MutableTTL: b.MutableTTL,
		ProbePath:  probePath,
	}, nil
}

func (rr *RemoteRepo) Close() error {
	if rr.Router != nil {
		return rr.Router.Close()
	}
	return nil
}

// ProbeUpstream implements registry.UpstreamProber.
func (rr *RemoteRepo) ProbeUpstream(ctx context.Context) (registry.UpstreamHealth, error) {
	p := rr.ProbePath
	if p == "" {
		p = "/"
	}
	return upstream.Probe(ctx, rr.Client, p), nil
}

// Fresh reports whether the cached copy at rel is within the mutable TTL.
func (rr *RemoteRepo) Fresh(rel string) bool {
	fi, err := rr.Store.Stat(rel)
	if err != nil {
		return false
	}
	return rr.MutableTTL > 0 && time.Since(fi.ModTime) < rr.MutableTTL
}

// FetchCached returns the bytes at cacheRel, fetching upstreamPath
// from the upstream when missing (immutable) or stale (mutable). On
// upstream failure a stale cached copy is served when available.
func (rr *RemoteRepo) FetchCached(ctx context.Context, cacheRel, upstreamPath string, mutable bool) ([]byte, error) {
	if rr.Store.Exists(cacheRel) && (!mutable || rr.Fresh(cacheRel)) {
		if b, err := rr.Store.Read(cacheRel); err == nil {
			return b, nil
		}
	}
	client := rr.Client
	if rr.Router != nil {
		client = rr.Router.For(upstreamPath)
	}
	resp, err := client.Get(ctx, upstreamPath)
	if err != nil {
		if b, rerr := rr.Store.Read(cacheRel); rerr == nil && !errors.Is(err, upstream.ErrNotFound) {
			return b, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = rr.Store.Write(cacheRel, body)
	return body, nil
}

// ServeCached is FetchCached + write the response.
func (rr *RemoteRepo) ServeCached(w http.ResponseWriter, r *http.Request, cacheRel, upstreamPath, contentType string, mutable bool) {
	body, err := rr.FetchCached(r.Context(), cacheRel, upstreamPath, mutable)
	if err != nil {
		UpstreamError(w, err)
		return
	}
	WriteBytes(w, r, http.StatusOK, contentType, body)
}

// PurgeCache implements registry.CachePurger. mutableDirs are the
// base-relative directories that hold TTL-bound metadata.
func (rr *RemoteRepo) Purge(opts registry.PurgeOptions, mutableDirs ...string) registry.PurgeStats {
	var out registry.PurgeStats
	dirs := mutableDirs
	if opts.All {
		dirs = []string{""}
	}
	for _, d := range dirs {
		n, b, errs := rr.Store.DeleteTree(d)
		out.PurgedFiles += n
		out.PurgedBytes += b
		for _, e := range errs {
			out.Errors = append(out.Errors, e.Error())
		}
	}
	return out
}

// StatsOf returns a basic Stats from a lister + store usage.
func StatsOf(ctx context.Context, l registry.PackageLister, s *Store) registry.Stats {
	var st registry.Stats
	if l != nil {
		if pkgs, err := l.ListPackages(ctx); err == nil {
			st.PackageCount = len(pkgs)
			for _, p := range pkgs {
				st.VersionCount += len(p.Versions)
			}
		}
	}
	st.BlobCount, st.TotalBytes = s.Usage("")
	return st
}
