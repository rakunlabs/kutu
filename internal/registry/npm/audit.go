package npm

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

// npm audit endpoints:
//
//	POST /-/npm/v1/security/advisories/bulk
//	POST /-/npm/v1/security/audits/quick
//
// Local answers "{}" (no advisories); Remote forwards the body to its
// default upstream; Virtual forwards to the first member answering 200.

const auditMaxBody = 32 << 20

func isAuditPath(p string) bool {
	return p == "/-/npm/v1/security/advisories/bulk" || p == "/-/npm/v1/security/audits/quick"
}

// serveEmptyAudit writes the "no advisories" response.
func serveEmptyAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "audit endpoints accept POST only")
		return
	}
	writeJSON(w, map[string]any{})
}

// auditForwarder proxies audit POSTs to the default upstream. The
// upstream.Client is GET-only, so this keeps its own http.Client and
// applies the repo-level auth itself.
type auditForwarder struct {
	base     string
	auth     *service.RegistryUpstreamAuth
	resolver registry.SecretResolver
	httpc    *http.Client
}

func newAuditForwarder(base string, auth *service.RegistryUpstreamAuth, resolver registry.SecretResolver, insecure bool) *auditForwarder {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &auditForwarder{
		base:     strings.TrimRight(base, "/"),
		auth:     auth,
		resolver: resolver,
		httpc:    &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}
}

// Close releases idle connections.
func (f *auditForwarder) Close() {
	if f != nil && f.httpc != nil {
		f.httpc.CloseIdleConnections()
	}
}

func (f *auditForwarder) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "audit endpoints accept POST only")
		return
	}
	if f == nil || f.base == "" {
		writeJSON(w, map[string]any{})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, auditMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, f.base+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	for _, h := range []string{"Content-Type", "Content-Encoding", "Accept", "Accept-Encoding", "User-Agent", "Npm-Command"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if err := f.applyAuth(r.Context(), req); err != nil {
		writeError(w, http.StatusBadGateway, "upstream auth: "+err.Error())
		return
	}
	resp, err := f.httpc.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream audit: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Encoding"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (f *auditForwarder) applyAuth(ctx context.Context, req *http.Request) error {
	if f.auth == nil {
		return nil
	}
	switch f.auth.Type {
	case service.RegistryAuthBasic:
		u, err := f.resolve(ctx, f.auth.Username)
		if err != nil {
			return err
		}
		p, err := f.resolve(ctx, f.auth.Password)
		if err != nil {
			return err
		}
		req.SetBasicAuth(u, p)
	case service.RegistryAuthBearer:
		t, err := f.resolve(ctx, f.auth.Token)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+t)
	case service.RegistryAuthHeader:
		v, err := f.resolve(ctx, f.auth.Value)
		if err != nil {
			return err
		}
		req.Header.Set(f.auth.Header, v)
	default:
		return fmt.Errorf("unsupported auth type %q", f.auth.Type)
	}
	return nil
}

func (f *auditForwarder) resolve(ctx context.Context, v string) (string, error) {
	if strings.HasPrefix(v, "secret://") {
		return "", fmt.Errorf("secret:// references are no longer supported; use raw://mount/path or config://key")
	}
	if f.resolver == nil || !(strings.HasPrefix(v, "raw://") || strings.HasPrefix(v, "config://")) {
		return v, nil
	}
	return f.resolver.ResolveSecret(ctx, v)
}

// serveAudit forwards the audit request to the first remote member
// that answers 200; falls back to "{}".
func (v *Virtual) serveAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "audit endpoints accept POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, auditMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	served := false
	v.ForEachMember(func(mem registry.Registry) bool {
		if mem.Kind() != service.RegistryKindRemote {
			return false
		}
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		rec := httptest.NewRecorder()
		mem.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return false
		}
		for _, h := range []string{"Content-Type", "Content-Encoding"} {
			if hv := rec.Header().Get(h); hv != "" {
				w.Header().Set(h, hv)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rec.Body.Bytes())
		served = true
		return true
	})
	if !served {
		writeJSON(w, map[string]any{})
	}
}
