package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/server/proxy"
	"github.com/rakunlabs/kutu/internal/service"
)

// proxyServerQueryValidator whitelists the columns a client may filter /
// sort proxy servers by (GET /api/v1/proxy?...).
var proxyServerQueryValidator = mustValidator("id", "name", "enabled", "listener_id", "protocol")

// proxyListenerQueryValidator whitelists listener filter/sort columns.
var proxyListenerQueryValidator = mustValidator("id", "name", "protocol", "port", "enabled")

// reconcileProxy reloads the live runner from the persisted listeners +
// servers. Called after any proxy CRUD mutation.
func (a *api) reconcileProxy(ctx context.Context) {
	if a.proxyMgr == nil {
		return
	}
	listeners, err := a.svc.ListProxyListeners(ctx, nil)
	if err != nil {
		return
	}
	servers, err := a.svc.ListProxyServers(ctx, nil)
	if err != nil {
		return
	}
	_ = a.proxyMgr.ReconcileAll(listeners, servers)
}

// proxyFeatureGuard rejects every proxy endpoint when the
// deployment-wide feature flag is off. We return 404 (not 403) so
// the SPA route gate and any external script see "feature does
// not exist here" — same semantics the vault uses when its own
// flag is flipped off.
func (a *api) proxyFeatureGuard(c *ada.Context) error {
	if !a.svc.ProxyEnabled(c.Request.Context()) {
		return errors.Join(errors.New("proxy feature is disabled"), service.ErrNotFound)
	}
	return nil
}

// Proxy HTTP layer. Per-entity CRUD on the kutu_proxy_server /
// kutu_proxy_listener tables + a passthrough to the proxy.Manager for
// validate/status. Every mutation reconciles the live runner from the
// freshly-persisted listener/server rows.

// listProxyServers returns every persisted ProxyServer. The full
// graph rides in the response so the UI doesn't need a second fetch
// per row to render the editor — proxy graphs are small (handful of
// nodes) compared to e.g. raw mounts metadata.
func (a *api) listProxyServers(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	q, err := parseListQuery(c, proxyServerQueryValidator)
	if err != nil {
		return err
	}
	servers, err := a.svc.ListProxyServers(c.Request.Context(), q)
	if err != nil {
		return err
	}
	if servers == nil {
		servers = []service.ProxyServer{}
	}
	return c.SetStatus(http.StatusOK).SendJSON(servers)
}

// getProxyServer returns one server by ID.
func (a *api) getProxyServer(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	srv, err := a.svc.GetProxyServer(c.Request.Context(), c.Request.PathValue("id"))
	if err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(srv)
}

// createProxyServer accepts a ProxyServer without an ID, assigns one,
// validates the graph and persists. Returns the assigned record so
// the SPA can switch its selection to it immediately.
func (a *api) createProxyServer(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	var srv service.ProxyServer
	if err := c.Bind(&srv); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	srv.ID = strings.TrimSpace(srv.ID)
	if srv.ID == "" {
		srv.ID = ulid.Make().String()
	}
	if a.proxyMgr != nil {
		if _, err := a.proxyMgr.Validate(srv); err != nil {
			return errors.Join(err, service.ErrBadRequest)
		}
	}
	if err := a.svc.CreateProxyServer(c.Request.Context(), &srv); err != nil {
		return err
	}
	a.reconcileProxy(c.Request.Context())
	return c.SetStatus(http.StatusCreated).SendJSON(srv)
}

// updateProxyServer replaces an existing entry. Body ID is taken
// from the path so the UI can't accidentally rewrite a different
// row by typo.
func (a *api) updateProxyServer(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	id := c.Request.PathValue("id")
	var srv service.ProxyServer
	if err := c.Bind(&srv); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	srv.ID = id
	if a.proxyMgr != nil {
		if _, err := a.proxyMgr.Validate(srv); err != nil {
			return errors.Join(err, service.ErrBadRequest)
		}
	}
	if err := a.svc.UpdateProxyServer(c.Request.Context(), &srv); err != nil {
		return err
	}
	a.reconcileProxy(c.Request.Context())
	return c.SetStatus(http.StatusOK).SendJSON(srv)
}

// deleteProxyServer drops the entry and reconciles the runner.
func (a *api) deleteProxyServer(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	if err := a.svc.DeleteProxyServer(c.Request.Context(), c.Request.PathValue("id")); err != nil {
		return err
	}
	a.reconcileProxy(c.Request.Context())
	return c.SendNoContent()
}

// validateProxyServer compiles the supplied graph without persisting
// it. Returns the compile result for the UI to surface (mainly the
// pipeline hash + listen address); compile errors come back as 400
// with a CompileError JSON so the SPA can highlight the bad node.
func (a *api) validateProxyServer(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	var srv service.ProxyServer
	if err := c.Bind(&srv); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	srv.ID = c.Request.PathValue("id")
	if a.proxyMgr == nil {
		return errors.New("proxy manager not configured")
	}
	res, err := a.proxyMgr.Validate(srv)
	if err != nil {
		var ce *proxy.CompileError
		if errors.As(err, &ce) {
			return c.SetStatus(http.StatusBadRequest).SendJSON(ce)
		}
		return errors.Join(err, service.ErrBadRequest)
	}
	return c.SetStatus(http.StatusOK).SendJSON(map[string]any{"ok": true, "pipeline": res})
}

// getProxyStatus exposes the live runner state. The shape is
// proxy.InstanceStatus per row; using `any` here keeps the api
// package free of an import on proxy types.
func (a *api) getProxyStatus(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	if a.proxyMgr == nil {
		return c.SetStatus(http.StatusOK).SendJSON([]any{})
	}
	return c.SetStatus(http.StatusOK).SendJSON(a.proxyMgr.Status())
}

// getProxyCatalog returns the registered middleware + handler kinds
// for the UI palette. The catalog is static (only changes with a
// rebuild) so no caching layer is needed beyond the registry itself.
func (a *api) getProxyCatalog(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(proxy.BuildCatalog())
}

// proxyTestRequest is the payload posted to /api/v1/proxy/test from
// the in-app HTTP playground. It is intentionally permissive:
// arbitrary scheme/host/port is allowed because pika is the
// deployment's gateway and the UI runs in the same trust boundary
// as the server, but we cap the timeout and the body size to keep
// a runaway loop from owning a worker forever.
type proxyTestRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// TimeoutMs caps the upstream wait. Default 10 000, hard ceiling
	// 60 000 enforced server-side; the form lets the user choose.
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

type proxyTestResponse struct {
	Status     int                 `json:"status"`
	StatusText string              `json:"status_text"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	DurationMs int64               `json:"duration_ms"`
	// Truncated is true when the upstream body exceeded the in-flight
	// cap; we still return what we read so the operator sees the
	// shape of the response.
	Truncated bool `json:"truncated,omitempty"`
}

const (
	proxyTestMaxBodyBytes  = 1 << 20 // 1 MiB; large enough for typical responses but bounded
	proxyTestDefaultTimeMs = 10_000
	proxyTestMaxTimeMs     = 60_000
)

// proxyTest forwards a single HTTP request from the SPA to any URL
// reachable from the server. The whole point is to let an operator
// sanity-check a proxy graph from the same UI where they built it:
// they pick the listener's port, hit /healthz or any route, and see
// the actual response shape + headers without leaving the page.
//
// CORS makes the in-browser equivalent impossible (the proxy
// listener is on a different port and almost never sets CORS
// allow-origin for the pika UI), so this server-side wrapper is the
// pragmatic alternative.
func (a *api) proxyTest(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	var req proxyTestRequest
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	if strings.TrimSpace(req.URL) == "" {
		return errors.Join(errors.New("url is required"), service.ErrBadRequest)
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = proxyTestDefaultTimeMs
	}
	if timeoutMs > proxyTestMaxTimeMs {
		timeoutMs = proxyTestMaxTimeMs
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	hReq, err := http.NewRequestWithContext(ctx, strings.ToUpper(req.Method), req.URL, body)
	if err != nil {
		return errors.Join(fmt.Errorf("build request: %w", err), service.ErrBadRequest)
	}
	for k, v := range req.Headers {
		hReq.Header.Set(k, v)
	}

	client := &http.Client{Timeout: time.Duration(timeoutMs) * time.Millisecond}
	start := time.Now()
	resp, err := client.Do(hReq)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		// Convert transport errors into a structured response so the
		// UI can render them inline rather than the generic 500.
		return c.SetStatus(http.StatusOK).SendJSON(proxyTestResponse{
			Status:     0,
			StatusText: err.Error(),
			Headers:    nil,
			Body:       "",
			DurationMs: dur,
		})
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, proxyTestMaxBodyBytes+1)
	buf, _ := io.ReadAll(limited)
	truncated := len(buf) > proxyTestMaxBodyBytes
	if truncated {
		buf = buf[:proxyTestMaxBodyBytes]
	}
	return c.SetStatus(http.StatusOK).SendJSON(proxyTestResponse{
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Headers:    resp.Header,
		Body:       string(buf),
		DurationMs: dur,
		Truncated:  truncated,
	})
}

// --- Listener CRUD ----------------------------------------------------------

// listProxyListeners returns every persisted ProxyListener.
func (a *api) listProxyListeners(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	q, err := parseListQuery(c, proxyListenerQueryValidator)
	if err != nil {
		return err
	}
	listeners, err := a.svc.ListProxyListeners(c.Request.Context(), q)
	if err != nil {
		return err
	}
	if listeners == nil {
		listeners = []service.ProxyListener{}
	}
	return c.SetStatus(http.StatusOK).SendJSON(listeners)
}

func (a *api) getProxyListener(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	ln, err := a.svc.GetProxyListener(c.Request.Context(), c.Request.PathValue("id"))
	if err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(ln)
}

func (a *api) createProxyListener(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	var ln service.ProxyListener
	if err := c.Bind(&ln); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	ln.ID = strings.TrimSpace(ln.ID)
	if ln.ID == "" {
		ln.ID = ulid.Make().String()
	}
	if a.proxyMgr != nil {
		if err := a.proxyMgr.ValidateListener(ln); err != nil {
			return errors.Join(err, service.ErrBadRequest)
		}
	}
	if err := a.svc.CreateProxyListener(c.Request.Context(), &ln); err != nil {
		return err
	}
	a.reconcileProxy(c.Request.Context())
	return c.SetStatus(http.StatusCreated).SendJSON(ln)
}

func (a *api) updateProxyListener(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	var ln service.ProxyListener
	if err := c.Bind(&ln); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	ln.ID = c.Request.PathValue("id")
	if a.proxyMgr != nil {
		if err := a.proxyMgr.ValidateListener(ln); err != nil {
			return errors.Join(err, service.ErrBadRequest)
		}
	}
	if err := a.svc.UpdateProxyListener(c.Request.Context(), &ln); err != nil {
		return err
	}
	a.reconcileProxy(c.Request.Context())
	return c.SetStatus(http.StatusOK).SendJSON(ln)
}

func (a *api) deleteProxyListener(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	id := c.Request.PathValue("id")
	// Reject delete if any graph still references this listener:
	// otherwise the graph would silently become unrunnable on next
	// reconcile.
	servers, err := a.svc.ListProxyServers(c.Request.Context(), nil)
	if err != nil {
		return err
	}
	using := []string{}
	for _, g := range servers {
		if g.ListenerID == id {
			using = append(using, g.ID)
		}
	}
	if len(using) > 0 {
		return errors.Join(
			fmt.Errorf("listener %q is still attached to %d graph(s): %s", id, len(using), strings.Join(using, ", ")),
			service.ErrBadRequest,
		)
	}
	if err := a.svc.DeleteProxyListener(c.Request.Context(), id); err != nil {
		return err
	}
	a.reconcileProxy(c.Request.Context())
	return c.SendNoContent()
}

// getProxyListenersStatus exposes the live listener bind state.
func (a *api) getProxyListenersStatus(c *ada.Context) error {
	if err := a.proxyFeatureGuard(c); err != nil {
		return err
	}
	if a.proxyMgr == nil {
		return c.SetStatus(http.StatusOK).SendJSON([]any{})
	}
	return c.SetStatus(http.StatusOK).SendJSON(a.proxyMgr.ListenersStatus())
}
