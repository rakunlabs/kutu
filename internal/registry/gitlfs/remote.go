package gitlfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Remote proxies downloads from an upstream LFS server (r.URL is the
// upstream LFS endpoint, e.g. https://github.com/org/repo.git/info/lfs).
type Remote struct {
	*pkgbase.RemoteRepo
	store    *Store
	base     string
	auth     *service.RegistryUpstreamAuth
	resolver registry.SecretResolver
	httpc    *http.Client
	pending  sync.Map // oid → pendingObject
	flight   sync.Map // oid → struct{}
}

type pendingObject struct {
	size    int64
	action  *action
	expires time.Time
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/objects/batch")
		if err != nil {
			return nil, err
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if r.InsecureSkipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		}
		return &Remote{
			RemoteRepo: base, store: &Store{base.Store},
			base: strings.TrimRight(r.URL, "/"), auth: r.Auth, resolver: deps.Resolver,
			httpc: &http.Client{Transport: tr, Timeout: 12 * time.Hour},
		}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func (rr *Remote) Close() error {
	rr.httpc.CloseIdleConnections()
	return rr.RemoteRepo.Close()
}

func (rr *Remote) resolveSecret(ctx context.Context, v string) (string, error) {
	if rr.resolver != nil && (strings.HasPrefix(v, "raw://") || strings.HasPrefix(v, "config://")) {
		return rr.resolver.ResolveSecret(ctx, v)
	}
	return v, nil
}

func (rr *Remote) applyAuth(ctx context.Context, req *http.Request) error {
	if rr.auth == nil {
		return nil
	}
	switch rr.auth.Type {
	case service.RegistryAuthBasic:
		u, err := rr.resolveSecret(ctx, rr.auth.Username)
		if err != nil {
			return err
		}
		p, err := rr.resolveSecret(ctx, rr.auth.Password)
		if err != nil {
			return err
		}
		req.SetBasicAuth(u, p)
	case service.RegistryAuthBearer:
		t, err := rr.resolveSecret(ctx, rr.auth.Token)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+t)
	case service.RegistryAuthHeader:
		v, err := rr.resolveSecret(ctx, rr.auth.Value)
		if err != nil {
			return err
		}
		req.Header.Set(rr.auth.Header, v)
	default:
		return fmt.Errorf("unsupported auth type %q", rr.auth.Type)
	}
	return nil
}

// upstreamBatch runs a download batch against the upstream and records
// the returned download actions.
func (rr *Remote) upstreamBatch(ctx context.Context, objs []batchObject) ([]batchObject, error) {
	body, err := json.Marshal(batchRequest{Operation: "download", Transfers: []string{"basic"}, Objects: objs, HashAlgo: "sha256"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rr.base+"/objects/batch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", MediaType)
	req.Header.Set("Content-Type", MediaType)
	if err := rr.applyAuth(ctx, req); err != nil {
		return nil, fmt.Errorf("upstream auth: %w", err)
	}
	resp, err := rr.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream batch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("upstream batch: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out batchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("upstream batch: %w", err)
	}
	if out.Transfer != "" && out.Transfer != "basic" {
		return nil, fmt.Errorf("upstream batch: unsupported transfer %q", out.Transfer)
	}
	now := time.Now()
	for _, o := range out.Objects {
		a := o.Actions["download"]
		if o.Error != nil || a == nil || !ValidOID(o.OID) {
			continue
		}
		exp := now.Add(time.Hour)
		if a.ExpiresIn > 0 {
			exp = now.Add(time.Duration(a.ExpiresIn) * time.Second)
		}
		if t, err := time.Parse(time.RFC3339, a.ExpiresAt); err == nil && t.Before(exp) {
			exp = t
		}
		rr.pending.Store(o.OID, pendingObject{size: o.Size, action: a, expires: exp.Add(-30 * time.Second)})
	}
	return out.Objects, nil
}

// lookup returns a fresh download action for oid, asking the upstream
// when none is recorded.
func (rr *Remote) lookup(ctx context.Context, oid string, size int64) (pendingObject, error) {
	if v, ok := rr.pending.Load(oid); ok {
		p := v.(pendingObject)
		if time.Now().Before(p.expires) && (size < 0 || p.size == size) {
			return p, nil
		}
		if size < 0 {
			size = p.size
		}
		rr.pending.Delete(oid)
	}
	if size < 0 {
		return pendingObject{}, errUpstreamMissing
	}
	objs, err := rr.upstreamBatch(ctx, []batchObject{{OID: oid, Size: size}})
	if err != nil {
		return pendingObject{}, err
	}
	for _, o := range objs {
		if o.OID == oid && o.Error != nil {
			return pendingObject{}, fmt.Errorf("%w: %s", errUpstreamMissing, o.Error.Message)
		}
	}
	if v, ok := rr.pending.Load(oid); ok {
		return v.(pendingObject), nil
	}
	return pendingObject{}, errUpstreamMissing
}

func (rr *Remote) hasObject(ctx context.Context, oid string, size int64) bool {
	if _, ok := rr.store.HasObject(oid, size); ok {
		return true
	}
	_, err := rr.lookup(ctx, oid, size)
	return err == nil
}

// openUpstream starts the download of an object.
func (rr *Remote) openUpstream(ctx context.Context, p pendingObject, rangeHdr string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.action.Href, nil)
	if err != nil {
		return nil, err
	}
	hasAuth := false
	for k, v := range p.action.Header {
		req.Header.Set(k, v)
		hasAuth = hasAuth || strings.EqualFold(k, "Authorization")
	}
	if !hasAuth && sameHost(p.action.Href, rr.base) {
		if err := rr.applyAuth(ctx, req); err != nil {
			return nil, err
		}
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := rr.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream download: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("upstream download: status %d", resp.StatusCode)
	}
	return resp, nil
}

func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.Host, ub.Host)
}

var errStarted = errors.New("response already started")

// fill downloads oid into the cache (verifying size + sha256), teeing
// the stream into the writer returned by start.
func (rr *Remote) fill(ctx context.Context, oid string, p pendingObject, start func() io.Writer) error {
	if _, loaded := rr.flight.LoadOrStore(oid, struct{}{}); loaded {
		return errBusy
	}
	defer rr.flight.Delete(oid)
	resp, err := rr.openUpstream(ctx, p, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	h := sha256.New()
	writers := []io.Writer{h}
	started := start != nil
	if started {
		writers = append(writers, &lenientWriter{w: start()})
	}
	cnt := &countWriter{}
	writers = append(writers, cnt)
	src := io.TeeReader(io.LimitReader(resp.Body, p.size), io.MultiWriter(writers...))
	err = rr.store.WriteStream(objectRel(oid), src, p.size)
	if err == nil && cnt.n != p.size {
		err = fmt.Errorf("size mismatch: got %d, want %d", cnt.n, p.size)
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != oid {
		err = errors.New("sha256 mismatch")
	}
	if err != nil {
		_ = rr.store.Delete(objectRel(oid))
		if started {
			return fmt.Errorf("%w: %w", errStarted, err)
		}
		return err
	}
	rr.pending.Delete(oid)
	return nil
}

var (
	errBusy            = errors.New("object is being cached by another request")
	errUpstreamMissing = errors.New("object not available upstream")
)

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

type lenientWriter struct {
	w      io.Writer
	failed bool
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	if !l.failed {
		if _, err := l.w.Write(p); err != nil {
			l.failed = true
		}
	}
	return len(p), nil
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := route(r.URL.Path)
	switch {
	case p == "/objects/batch" && r.Method == http.MethodPost:
		rr.batch(w, r)
	case p == "/objects/verify" && r.Method == http.MethodPost:
		verify(w, r, rr.store)
	case strings.HasPrefix(p, "/objects/") && pkgbase.IsRead(r):
		oid, ok := objectsOID(p)
		if !ok {
			lfsError(w, http.StatusNotFound, "not found")
			return
		}
		rr.serveObject(w, r, oid)
	case strings.HasPrefix(p, "/locks"):
		if p == "/locks" && r.Method == http.MethodGet {
			lfsJSON(w, r, http.StatusOK, map[string]any{"locks": []Lock{}})
			return
		}
		if p == "/locks/verify" && r.Method == http.MethodPost {
			lfsJSON(w, r, http.StatusOK, map[string]any{"ours": []Lock{}, "theirs": []Lock{}})
			return
		}
		lfsError(w, http.StatusMethodNotAllowed, "remote registry is read-only")
	default:
		if !pkgbase.IsRead(r) {
			lfsError(w, http.StatusMethodNotAllowed, "remote registry is read-only")
			return
		}
		lfsError(w, http.StatusNotFound, "not found")
	}
}

func (rr *Remote) batch(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBatch(w, r)
	if !ok {
		return
	}
	if req.Operation != "download" {
		lfsError(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	out := make([]batchObject, len(req.Objects))
	var missing []batchObject
	idx := map[string][]int{}
	for i, o := range req.Objects {
		out[i] = batchObject{OID: o.OID, Size: o.Size}
		switch {
		case !ValidOID(o.OID) || o.Size < 0:
			out[i].Error = &objError{Code: http.StatusUnprocessableEntity, Message: "invalid object"}
		default:
			if _, ok := rr.store.HasObject(o.OID, o.Size); ok {
				out[i].Actions = map[string]*action{"download": newAction(r, objectHref(r, o.OID))}
				continue
			}
			if _, seen := idx[o.OID]; !seen {
				missing = append(missing, batchObject{OID: o.OID, Size: o.Size})
			}
			idx[o.OID] = append(idx[o.OID], i)
		}
	}
	if len(missing) > 0 {
		ups, err := rr.upstreamBatch(r.Context(), missing)
		if err != nil {
			lfsError(w, http.StatusBadGateway, err.Error())
			return
		}
		resolved := map[string]*objError{}
		for _, o := range ups {
			if o.Error != nil {
				resolved[o.OID] = o.Error
			} else if o.Actions["download"] != nil {
				resolved[o.OID] = nil
			}
		}
		for oid, is := range idx {
			e, ok := resolved[oid]
			for _, i := range is {
				switch {
				case !ok:
					out[i].Error = &objError{Code: http.StatusNotFound, Message: "object does not exist"}
				case e != nil:
					out[i].Error = e
				default:
					href := objectHref(r, oid) + "?size=" + strconv.FormatInt(out[i].Size, 10)
					out[i].Actions = map[string]*action{"download": newAction(r, href)}
				}
			}
		}
	}
	lfsJSON(w, r, http.StatusOK, batchResponse{Transfer: "basic", Objects: out, HashAlgo: "sha256"})
}

func (rr *Remote) serveObject(w http.ResponseWriter, r *http.Request, oid string) {
	if _, ok := rr.flight.Load(oid); !ok && serveObject(w, r, rr.store, oid) {
		return
	}
	size := int64(-1)
	if s, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64); err == nil && s >= 0 {
		size = s
	}
	pend, err := rr.lookup(r.Context(), oid, size)
	if err != nil {
		if errors.Is(err, errUpstreamMissing) {
			lfsError(w, http.StatusNotFound, "object does not exist")
			return
		}
		lfsError(w, http.StatusBadGateway, err.Error())
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(pend.size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("Range") == "" {
		err = rr.fill(context.WithoutCancel(r.Context()), oid, pend, func() io.Writer {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.FormatInt(pend.size, 10))
			w.WriteHeader(http.StatusOK)
			return w
		})
		if err == nil || errors.Is(err, errStarted) {
			return
		}
		if !errors.Is(err, errBusy) {
			lfsError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	resp, err := rr.openUpstream(r.Context(), pend, r.Header.Get("Range"))
	if err != nil {
		lfsError(w, http.StatusBadGateway, err.Error())
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

// ── Remote optional interfaces ──

// ProbeUpstream posts an empty download batch to the upstream.
func (rr *Remote) ProbeUpstream(ctx context.Context) (registry.UpstreamHealth, error) {
	start := time.Now()
	h := registry.UpstreamHealth{URL: rr.base + "/objects/batch"}
	_, err := rr.upstreamBatch(ctx, []batchObject{})
	h.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		h.Error = err.Error()
		return h, nil
	}
	h.OK, h.StatusCode = true, http.StatusOK
	return h, nil
}

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return []registry.PackageSummary{}, nil
}

func (rr *Remote) Stats(context.Context) (registry.Stats, error) {
	return objectStats(rr.store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return objectDetail(rr.store, name)
}

// PurgeCache drops cached objects (objects are immutable, so only a
// deep purge removes anything).
func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	if !opts.All {
		return registry.PurgeStats{}, nil
	}
	return rr.Purge(opts), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

// DeleteVersion evicts a cached object (name = oid).
func (rr *Remote) DeleteVersion(_ context.Context, name, _ string) error {
	if _, ok := rr.store.HasObject(name, -1); !ok {
		return registry.ErrPackageNotFound
	}
	return rr.store.Delete(objectRel(name))
}

// Prefetch caches object name (= oid). version may carry the object
// size, required when the upstream has not announced the object yet.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if !ValidOID(name) {
		return registry.ErrInvalidPackageName
	}
	if _, ok := rr.store.HasObject(name, -1); ok {
		return nil
	}
	size := int64(-1)
	if s, err := strconv.ParseInt(version, 10, 64); err == nil {
		size = s
	}
	p, err := rr.lookup(ctx, name, size)
	if err != nil {
		return err
	}
	if err := rr.fill(ctx, name, p, nil); err != nil && !errors.Is(err, errBusy) {
		return err
	}
	return nil
}
