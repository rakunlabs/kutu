// Package vhost implements kutu's shared HTTP listener layer. Any
// HTTP-based subsystem (registry listeners, S3 serve, WebDAV serve)
// can publish "bindings" — a bind address + optional hostname +
// optional TLS certificate + an http.Handler. Bindings that land on
// the same host:port share a single net.Listener; requests are routed
// to the matching binding by the request's Host header, and TLS
// certificates are selected per-hostname via SNI.
//
// Rules per port group:
//
//   - Hostnames must be unique. "" is the catch-all (at most one).
//     A "*.example.com" wildcard matches exactly one extra label.
//   - The group serves TLS when any binding carries a certificate.
//     In a TLS group every binding must carry its own certificate
//     (SNI selects the right one); a plain binding on a TLS port is
//     rejected with a per-binding error, not a dead listener.
//   - TLS is entirely optional: a group with no certificates serves
//     plain HTTP and still routes by Host header, which is the
//     normal deployment mode behind a TLS-terminating reverse proxy.
//
// Reconcile model: every owner subsystem replaces its own binding set
// with SetBindings(owner, ...). The manager merges all owners, diffs
// the resulting port groups against the running listeners, and only
// rebinds a group when its bind-level config changed (address or
// TLS-ness). Routing tables and certificates hot-swap without
// dropping connections.
package vhost

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Binding is one published virtual host: where to listen, which
// hostname to answer for, and the handler that serves it.
type Binding struct {
	// Owner + ID identify the binding for status reporting. ID must be
	// unique within an owner.
	Owner string
	ID    string
	Name  string

	// Host is the bind address ("" = 0.0.0.0). Port is required.
	Host string
	Port int

	// Hostname this binding answers for. Matching is case-insensitive
	// against the request Host header (port stripped) and the TLS SNI
	// server name. Empty = catch-all for the group. A leading "*."
	// makes it a single-label wildcard.
	Hostname string

	// Optional TLS keypair (PEM). Both or neither.
	TLSCertPEM string
	TLSKeyPEM  string

	// Handler serves matched requests.
	Handler http.Handler
}

// Status is the runtime state of one binding.
type Status struct {
	Owner    string `json:"owner"`
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Address  string `json:"address"`
	Hostname string `json:"hostname,omitempty"`
	TLS      bool   `json:"tls"`
	Running  bool   `json:"running"`
	// Shared reports whether other bindings share this port.
	Shared bool   `json:"shared,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Manager owns every shared listener. One instance per process,
// shared by all owner subsystems.
type Manager struct {
	mu     sync.Mutex
	appCtx context.Context

	// owners holds the desired bindings per owner subsystem, in the
	// order the owner supplied them (status output preserves it).
	owners map[string][]Binding

	// groups holds the running listeners keyed by bind address.
	groups map[string]*group

	// status keyed by owner + "\x00" + id.
	status map[string]Status
}

// NewManager constructs an empty manager. appCtx bounds the lifetime
// of every listener.
func NewManager(appCtx context.Context) *Manager {
	return &Manager{
		appCtx: appCtx,
		owners: map[string][]Binding{},
		groups: map[string]*group{},
		status: map[string]Status{},
	}
}

// SetBindings replaces owner's binding set and reconciles the shared
// listeners. Safe to call repeatedly and from any goroutine.
func (m *Manager) SetBindings(owner string, bindings []Binding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range bindings {
		bindings[i].Owner = owner
	}
	if len(bindings) == 0 {
		delete(m.owners, owner)
	} else {
		m.owners[owner] = bindings
	}
	m.reconcileLocked()
}

// Status returns the runtime state of every binding published by
// owner, in the order they were supplied.
func (m *Manager) Status(owner string) []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	bs := m.owners[owner]
	out := make([]Status, 0, len(bs))
	for i := range bs {
		if st, ok := m.status[statusKey(bs[i].Owner, bs[i].ID)]; ok {
			out = append(out, st)
		}
	}
	return out
}

// Stop tears down every listener. Called on shutdown.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for addr, g := range m.groups {
		g.shutdown()
		delete(m.groups, addr)
	}
}

// ── reconcile ──

// route is one resolved (validated) binding inside a group's routing
// table.
type route struct {
	binding Binding
	cert    *tls.Certificate // nil in plain groups
}

// routeTable is the immutable per-group routing state, swapped
// atomically on reconcile.
type routeTable struct {
	// exact hostname → route; key "" is the catch-all.
	byHost map[string]*route
	// wildcard suffix (".example.com") → route, for "*.example.com".
	wildcard map[string]*route
	// defaultCert answers SNI misses: catch-all's cert, else the
	// lexically-first hostname's cert.
	defaultCert *tls.Certificate
}

// group is one running listener shared by every binding on its
// address.
type group struct {
	addr   string
	useTLS bool
	routes atomic.Pointer[routeTable]

	srv    *http.Server
	ln     net.Listener
	cancel context.CancelFunc
}

func (g *group) shutdown() {
	if g.cancel != nil {
		g.cancel()
	}
	if g.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.srv.Shutdown(ctx)
	}
}

// reconcileLocked rebuilds the desired group set from all owners and
// diffs it against the running groups. Must be called with m.mu held.
func (m *Manager) reconcileLocked() {
	m.status = map[string]Status{}

	// Merge every owner's bindings, grouped by bind address. Owner
	// iteration order is made deterministic so conflict resolution
	// (first wins) is stable across reconciles.
	ownerNames := make([]string, 0, len(m.owners))
	for o := range m.owners {
		ownerNames = append(ownerNames, o)
	}
	sort.Strings(ownerNames)

	desired := map[string][]Binding{}
	for _, o := range ownerNames {
		for _, b := range m.owners[o] {
			addr := bindAddr(b.Host, b.Port)
			desired[addr] = append(desired[addr], b)
		}
	}

	// Stop groups whose address disappeared.
	for addr, g := range m.groups {
		if _, ok := desired[addr]; !ok {
			g.shutdown()
			delete(m.groups, addr)
		}
	}

	for addr, bindings := range desired {
		m.reconcileGroup(addr, bindings)
	}
}

// reconcileGroup validates one address's bindings, builds the routing
// table, and starts / hot-updates / restarts the listener as needed.
func (m *Manager) reconcileGroup(addr string, bindings []Binding) {
	shared := len(bindings) > 1
	useTLS := false
	for i := range bindings {
		if bindings[i].TLSCertPEM != "" || bindings[i].TLSKeyPEM != "" {
			useTLS = true
			break
		}
	}

	rt := &routeTable{byHost: map[string]*route{}, wildcard: map[string]*route{}}
	valid := 0

	for i := range bindings {
		b := bindings[i]
		st := Status{
			Owner:    b.Owner,
			ID:       b.ID,
			Name:     b.Name,
			Address:  addr,
			Hostname: b.Hostname,
			TLS:      useTLS,
			Shared:   shared,
		}

		rte, err := buildRoute(b, useTLS)
		if err == nil {
			err = installRoute(rt, rte)
		}
		if err != nil {
			st.Error = err.Error()
			m.status[statusKey(b.Owner, b.ID)] = st
			slog.Warn("vhost: binding rejected", "owner", b.Owner, "id", b.ID, "addr", addr, "hostname", b.Hostname, "error", err)
			continue
		}
		valid++
		// Running is stamped below once the listener state is known.
		m.status[statusKey(b.Owner, b.ID)] = st
	}

	// Pick the SNI fallback certificate: catch-all first, then the
	// lexically-first hostname.
	if useTLS {
		if r, ok := rt.byHost[""]; ok {
			rt.defaultCert = r.cert
		} else {
			names := make([]string, 0, len(rt.byHost))
			for h := range rt.byHost {
				names = append(names, h)
			}
			sort.Strings(names)
			if len(names) > 0 {
				rt.defaultCert = rt.byHost[names[0]].cert
			} else {
				wild := make([]string, 0, len(rt.wildcard))
				for h := range rt.wildcard {
					wild = append(wild, h)
				}
				sort.Strings(wild)
				if len(wild) > 0 {
					rt.defaultCert = rt.wildcard[wild[0]].cert
				}
			}
		}
	}

	g, running := m.groups[addr]
	if running && g.useTLS != useTLS {
		// TLS-ness flipped: the listener must be rebuilt.
		g.shutdown()
		delete(m.groups, addr)
		running = false
	}

	if valid == 0 {
		// Nothing routable — don't hold the port.
		if running {
			g.shutdown()
			delete(m.groups, addr)
		}
		return
	}

	if running {
		g.routes.Store(rt)
		m.markGroup(bindings, func(st *Status) { st.Running = st.Error == "" })
		return
	}

	g, err := m.startGroup(addr, useTLS, rt)
	if err != nil {
		m.markGroup(bindings, func(st *Status) {
			if st.Error == "" {
				st.Error = err.Error()
			}
		})
		slog.Warn("vhost: listener not started", "addr", addr, "error", err)
		return
	}
	m.groups[addr] = g
	m.markGroup(bindings, func(st *Status) { st.Running = st.Error == "" })
}

// markGroup applies fn to the status entry of every binding in the
// slice. Must be called with m.mu held.
func (m *Manager) markGroup(bindings []Binding, fn func(*Status)) {
	for i := range bindings {
		key := statusKey(bindings[i].Owner, bindings[i].ID)
		st, ok := m.status[key]
		if !ok {
			continue
		}
		fn(&st)
		m.status[key] = st
	}
}

// startGroup binds the address and serves in a goroutine.
func (m *Manager) startGroup(addr string, useTLS bool, rt *routeTable) (*group, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	g := &group{addr: addr, useTLS: useTLS}
	g.routes.Store(rt)

	srv := &http.Server{Handler: http.HandlerFunc(g.dispatch)}
	if useTLS {
		srv.TLSConfig = &tls.Config{GetCertificate: g.getCertificate}
	}
	g.srv = srv
	g.ln = ln

	ctx, cancel := context.WithCancel(m.appCtx)
	g.cancel = cancel

	go func() {
		slog.Info("vhost: listener started", "addr", addr, "tls", useTLS)
		var serveErr error
		if useTLS {
			serveErr = srv.ServeTLS(ln, "", "")
		} else {
			serveErr = srv.Serve(ln)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			slog.Error("vhost: listener failed", "addr", addr, "error", serveErr)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	return g, nil
}

// dispatch routes one request by Host header.
func (g *group) dispatch(w http.ResponseWriter, r *http.Request) {
	rt := g.routes.Load()
	if rt == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no routes configured")
		return
	}
	if rte := rt.match(hostOnly(r.Host)); rte != nil {
		rte.binding.Handler.ServeHTTP(w, r)
		return
	}
	// 421 tells well-behaved HTTP clients they reached the wrong
	// virtual host.
	writeJSONError(w, http.StatusMisdirectedRequest, "no virtual host configured for "+hostOnly(r.Host))
}

// getCertificate selects the binding certificate matching the SNI
// server name, falling back to the group default.
func (g *group) getCertificate(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	rt := g.routes.Load()
	if rt == nil {
		return nil, errors.New("vhost: no routes configured")
	}
	if rte := rt.match(strings.ToLower(chi.ServerName)); rte != nil && rte.cert != nil {
		return rte.cert, nil
	}
	if rt.defaultCert != nil {
		return rt.defaultCert, nil
	}
	return nil, fmt.Errorf("vhost: no certificate for %q", chi.ServerName)
}

// match resolves a lowercase hostname to a route: exact, then
// wildcard, then catch-all.
func (rt *routeTable) match(host string) *route {
	if host != "" {
		if r, ok := rt.byHost[host]; ok {
			return r
		}
		if i := strings.IndexByte(host, '.'); i > 0 {
			if r, ok := rt.wildcard[host[i:]]; ok {
				return r
			}
		}
	}
	if r, ok := rt.byHost[""]; ok {
		return r
	}
	return nil
}

// buildRoute validates one binding against the group's TLS mode and
// parses its certificate.
func buildRoute(b Binding, groupTLS bool) (*route, error) {
	if b.Handler == nil {
		return nil, errors.New("no handler")
	}
	if (b.TLSCertPEM == "") != (b.TLSKeyPEM == "") {
		return nil, errors.New("TLS requires both a certificate and a key")
	}
	r := &route{binding: b}
	if b.TLSCertPEM != "" {
		cert, err := tls.X509KeyPair([]byte(b.TLSCertPEM), []byte(b.TLSKeyPEM))
		if err != nil {
			return nil, fmt.Errorf("invalid TLS keypair: %w", err)
		}
		r.cert = &cert
	} else if groupTLS {
		return nil, errors.New("port serves TLS: this binding needs its own certificate or a different port")
	}
	return r, nil
}

// installRoute inserts a route into the table, rejecting hostname
// collisions.
func installRoute(rt *routeTable, r *route) error {
	host := strings.ToLower(strings.TrimSpace(r.binding.Hostname))
	if strings.HasPrefix(host, "*.") {
		suffix := host[1:] // ".example.com"
		if _, dup := rt.wildcard[suffix]; dup {
			return fmt.Errorf("hostname %q already bound on this port", host)
		}
		rt.wildcard[suffix] = r
		return nil
	}
	if _, dup := rt.byHost[host]; dup {
		if host == "" {
			return errors.New("another binding is already the catch-all on this port")
		}
		return fmt.Errorf("hostname %q already bound on this port", host)
	}
	rt.byHost[host] = r
	return nil
}

// ── helpers ──

func statusKey(owner, id string) string { return owner + "\x00" + id }

func bindAddr(host string, port int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// hostOnly strips an optional :port from a Host header value and
// lowercases the rest.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.TrimSuffix(hostport, ":"))
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, msg)
}
