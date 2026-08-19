// Package serve wires kutu's built-in file-serving servers (FTP, SFTP,
// TFTP, WebDAV, S3) to the persisted ServeSettings and the live
// raw-mount table. A single Manager owns any number of server
// instances — several of the same protocol on different ports are fine.
// Reconcile is called at boot and after every settings / raw-mount
// mutation to bring the running instances in line with the desired
// configuration.
//
// Shares are resolved against the raw-mount handler on every reconcile,
// so a share that points at "data/releases" serves the live filesystem
// backing the "data" raw mount. Users and shares are shared pools; each
// server instance exposes either all shares or the subset named in its
// entry (TFTP is anonymous and read-only by protocol design and so
// ignores the user list).
package serve

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
	"github.com/rakunlabs/kutu/internal/serve/s3serve"
	"github.com/rakunlabs/kutu/internal/serve/sftpserve"
	"github.com/rakunlabs/kutu/internal/serve/tftpserve"
	"github.com/rakunlabs/kutu/internal/serve/webdavserve"
	"github.com/rakunlabs/kutu/internal/server/vhost"
	"github.com/rakunlabs/kutu/internal/service"
)

// vhostOwner is the owner key this manager publishes S3/WebDAV
// bindings under on the shared vhost layer.
const vhostOwner = "serve"

// Default listen ports, mirrored from the individual server packages so
// the UI status view can show the effective address even before a
// server binds.
const (
	defaultFTPPort    = 2121
	defaultSFTPPort   = 2222
	defaultTFTPPort   = 69
	defaultWebDAVPort = 9119
	defaultS3Port     = 9000
)

// DefaultPort returns the default listen port for a serve protocol.
func DefaultPort(protocol string) int {
	switch protocol {
	case "ftp":
		return defaultFTPPort
	case "sftp":
		return defaultSFTPPort
	case "tftp":
		return defaultTFTPPort
	case "webdav":
		return defaultWebDAVPort
	case "s3":
		return defaultS3Port
	}
	return 0
}

// MountResolver resolves a raw-mount prefix to its live filesystem. The
// api.RawHandler satisfies it; keeping it an interface avoids a server →
// api import cycle and lets shares re-resolve against the hot-reloaded
// mount table on every reconcile.
type MountResolver interface {
	MountFS(prefix string) (rawfs.RawFS, bool)
}

// Status is the runtime state of one serve server instance, surfaced to
// the UI. Hostname / TLS / SharedPort are only meaningful for the
// HTTP-based protocols (S3, WebDAV) that run on the shared vhost layer.
type Status struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Protocol   string `json:"protocol"`
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	Address    string `json:"address,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	TLS        bool   `json:"tls,omitempty"`
	SharedPort bool   `json:"shared_port,omitempty"`
	Error      string `json:"error,omitempty"`
}

// instance is one running (or attempted) server. The function fields
// abstract over the concrete server types so reconciliation is
// uniform. Vhost-backed instances (S3, WebDAV) additionally carry the
// binding they publish so an unchanged instance can re-submit it on
// every reconcile.
type instance struct {
	sig          string // bind-config signature; a change forces a rebind
	cancel       context.CancelFunc
	stop         func()
	updateShares func([]ftpserve.Share)
	updateUsers  func([]ftpserve.User) // nil for TFTP (anonymous)
	binding      *vhost.Binding        // nil for self-listening protocols
}

func (in *instance) shutdown() {
	if in.cancel != nil {
		in.cancel()
	}
	if in.stop != nil {
		in.stop()
	}
}

// Manager owns the lifecycle of every serve server instance. FTP,
// SFTP and TFTP bind their own listeners; the HTTP-based protocols
// (S3, WebDAV) publish bindings onto the shared vhost layer instead,
// which lets several instances — and registry listeners — share one
// port split by hostname, with optional per-hostname TLS.
type Manager struct {
	mu       sync.Mutex
	appCtx   context.Context
	resolver MountResolver
	vhosts   *vhost.Manager

	// onSFTPKey persists an auto-generated SFTP host key for the given
	// server instance so it survives restarts. May be nil.
	onSFTPKey func(serverID, pem string)

	instances map[string]*instance
	order     []string // instance IDs in configured order, for stable status output
	status    map[string]Status
}

// NewManager constructs a Manager. resolver supplies the raw-mount
// filesystems shares are built from; vhosts is the shared HTTP
// listener layer S3/WebDAV instances publish to; onSFTPKey (may be
// nil) persists a generated SFTP host key for a server instance.
func NewManager(appCtx context.Context, resolver MountResolver, vhosts *vhost.Manager, onSFTPKey func(serverID, pem string)) *Manager {
	return &Manager{
		appCtx:    appCtx,
		resolver:  resolver,
		vhosts:    vhosts,
		onSFTPKey: onSFTPKey,
		instances: map[string]*instance{},
		status:    map[string]Status{},
	}
}

// Reconcile brings the running instances in line with cfg. It is safe
// to call repeatedly; an instance whose bind config is unchanged keeps
// its listener and only refreshes its shares/users.
func (m *Manager) Reconcile(cfg *service.ServeSettings) {
	if cfg == nil {
		cfg = &service.ServeSettings{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	allShares := m.buildShares(cfg.Shares)
	users := buildUsers(cfg.Users)

	desired := make(map[string]service.ServeServerEntry, len(cfg.Servers))
	order := make([]string, 0, len(cfg.Servers))
	for i, e := range cfg.Servers {
		if e.ID == "" {
			// Should not happen (the API assigns IDs), but never let two
			// entries collide on the empty key.
			e.ID = e.Protocol + "-" + strconv.Itoa(i)
		}
		if _, dup := desired[e.ID]; dup {
			slog.Warn("serve: duplicate server id ignored", "id", e.ID)
			continue
		}
		desired[e.ID] = e
		order = append(order, e.ID)
	}

	// Stop instances that were removed from the configuration.
	for id, inst := range m.instances {
		if _, ok := desired[id]; !ok {
			inst.shutdown()
			delete(m.instances, id)
		}
	}
	for id := range m.status {
		if _, ok := desired[id]; !ok {
			delete(m.status, id)
		}
	}
	m.order = order

	bindings := make([]vhost.Binding, 0, len(order))
	for _, id := range order {
		e := desired[id]
		if isVhostProtocol(e.Protocol) {
			bindings = append(bindings, m.reconcileVhostInstance(e, filterShares(allShares, e.Shares), users)...)
			continue
		}
		m.reconcileInstance(e, filterShares(allShares, e.Shares), users)
	}
	if m.vhosts != nil {
		m.vhosts.SetBindings(vhostOwner, bindings)
	}
}

// isVhostProtocol reports whether a protocol serves HTTP through the
// shared vhost layer instead of binding its own listener.
func isVhostProtocol(protocol string) bool {
	return protocol == "s3" || protocol == "webdav"
}

// Status returns a snapshot of each instance's runtime state in
// configured order. For vhost-backed protocols the live bind state
// (running / shared / TLS / error) is overlaid from the vhost layer.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	var vs map[string]vhost.Status
	if m.vhosts != nil {
		list := m.vhosts.Status(vhostOwner)
		vs = make(map[string]vhost.Status, len(list))
		for _, s := range list {
			vs[s.ID] = s
		}
	}

	out := make([]Status, 0, len(m.order))
	for _, id := range m.order {
		st, ok := m.status[id]
		if !ok {
			continue
		}
		if b, ok := vs[st.ID]; ok {
			st.Running = b.Running
			st.Address = b.Address
			st.TLS = b.TLS
			st.SharedPort = b.Shared
			if b.Error != "" {
				st.Error = b.Error
			}
		}
		out = append(out, st)
	}
	return out
}

// Stop tears down every running instance. Called on shutdown. The
// vhost layer is shared with other subsystems and is stopped by its
// own owner (server.Start), not here — we only retract our bindings.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, inst := range m.instances {
		inst.shutdown()
		delete(m.instances, id)
	}
	if m.vhosts != nil {
		m.vhosts.SetBindings(vhostOwner, nil)
	}
}

// reconcileInstance brings a single configured server entry in line:
// hot-update when the bind config is unchanged, otherwise stop and (if
// enabled) start fresh. Must be called with m.mu held.
func (m *Manager) reconcileInstance(e service.ServeServerEntry, shares []ftpserve.Share, users []ftpserve.User) {
	st := Status{
		ID:       e.ID,
		Name:     e.Name,
		Protocol: e.Protocol,
		Enabled:  e.Enabled,
		Address:  entryAddr(e),
	}

	sig := entrySignature(e)
	if inst, ok := m.instances[e.ID]; ok {
		if e.Enabled && inst.sig == sig {
			inst.updateShares(shares)
			if inst.updateUsers != nil {
				inst.updateUsers(users)
			}
			st.Running = true
			m.status[e.ID] = st
			return
		}
		inst.shutdown()
		delete(m.instances, e.ID)
	}

	if !e.Enabled {
		m.status[e.ID] = st
		return
	}

	// SFTP binds its own listener inside NewServer; preflight FTP/TFTP
	// so a taken port yields a precise error instead of a dead server.
	// (S3/WebDAV never reach here — the vhost layer owns their bind.)
	if e.Protocol == "ftp" || e.Protocol == "tftp" {
		preflight := preflightTCP
		if e.Protocol == "tftp" {
			preflight = preflightUDP
		}
		if err := preflight(st.Address); err != nil {
			st.Error = err.Error()
			m.status[e.ID] = st
			slog.Warn("serve server not started", "protocol", e.Protocol, "id", e.ID, "address", st.Address, "error", err)
			return
		}
	}

	inst, sig, err := m.startInstance(e, shares, users)
	if err != nil {
		st.Error = err.Error()
		m.status[e.ID] = st
		slog.Warn("serve server not started", "protocol", e.Protocol, "id", e.ID, "address", st.Address, "error", err)
		return
	}
	inst.sig = sig
	m.instances[e.ID] = inst
	st.Running = true
	m.status[e.ID] = st
}

// startInstance builds and starts the concrete server for an entry and
// returns its instance wrapper plus the applied bind signature (which
// may differ from the input entry's — e.g. a freshly generated SFTP
// host key is folded in so the next reconcile does not rebind).
func (m *Manager) startInstance(e service.ServeServerEntry, shares []ftpserve.Share, users []ftpserve.User) (*instance, string, error) {
	ctx, cancel := context.WithCancel(m.appCtx)

	switch e.Protocol {
	case "ftp":
		cfg := valueOr(e.FTP)
		e.FTP = &cfg
		srv, err := ftpserve.NewServer(&cfg, shares, users)
		if err != nil {
			cancel()
			return nil, "", err
		}
		srv.Start(ctx)
		return &instance{cancel: cancel, stop: srv.Stop, updateShares: srv.UpdateShares, updateUsers: srv.UpdateUsers}, entrySignature(e), nil

	case "sftp":
		cfg := valueOr(e.SFTP)
		e.SFTP = &cfg
		serverID := e.ID
		onKey := func(pem string) {
			// Remember the generated key on the applied config so the
			// signature computed below already includes it, and persist
			// it so it survives restarts.
			cfg.HostKeyPEM = pem
			if m.onSFTPKey != nil {
				m.onSFTPKey(serverID, pem)
			}
		}
		srv, err := sftpserve.NewServer(&cfg, shares, users, onKey)
		if err != nil {
			cancel()
			return nil, "", err
		}
		srv.Start(ctx)
		return &instance{cancel: cancel, stop: srv.Stop, updateShares: srv.UpdateShares, updateUsers: srv.UpdateUsers}, entrySignature(e), nil

	case "tftp":
		cfg := valueOr(e.TFTP)
		e.TFTP = &cfg
		srv, err := tftpserve.NewServer(&cfg, shares)
		if err != nil {
			cancel()
			return nil, "", err
		}
		srv.Start(ctx, &cfg)
		return &instance{cancel: cancel, stop: srv.Stop, updateShares: srv.UpdateShares}, entrySignature(e), nil

	}

	cancel()
	return nil, "", &unknownProtocolError{protocol: e.Protocol}
}

// reconcileVhostInstance brings an HTTP-based (S3/WebDAV) entry in
// line and returns the vhost binding it publishes (empty when the
// entry is disabled or failed to build). The vhost layer decides
// whether the binding gets its own listener or shares a port with
// other hostnames. Must be called with m.mu held.
func (m *Manager) reconcileVhostInstance(e service.ServeServerEntry, shares []ftpserve.Share, users []ftpserve.User) []vhost.Binding {
	st := Status{
		ID:       e.ID,
		Name:     e.Name,
		Protocol: e.Protocol,
		Enabled:  e.Enabled,
		Address:  entryAddr(e),
		Hostname: entryHostname(e),
	}

	sig := entrySignature(e)
	if inst, ok := m.instances[e.ID]; ok {
		if e.Enabled && inst.sig == sig && inst.binding != nil {
			inst.updateShares(shares)
			if inst.updateUsers != nil {
				inst.updateUsers(users)
			}
			m.status[e.ID] = st
			return []vhost.Binding{*inst.binding}
		}
		inst.shutdown()
		delete(m.instances, e.ID)
	}

	if !e.Enabled {
		m.status[e.ID] = st
		return nil
	}

	var (
		b    vhost.Binding
		inst *instance
	)
	switch e.Protocol {
	case "s3":
		cfg := valueOr(e.S3)
		e.S3 = &cfg
		srv, err := s3serve.NewServer(&cfg, shares, users)
		if err != nil {
			st.Error = err.Error()
			m.status[e.ID] = st
			slog.Warn("serve server not started", "protocol", e.Protocol, "id", e.ID, "error", err)
			return nil
		}
		b = vhost.Binding{
			Host: cfg.Host, Port: cfg.Port,
			Hostname:   cfg.Hostname,
			TLSCertPEM: cfg.TLSCertPEM, TLSKeyPEM: cfg.TLSKeyPEM,
			Handler: srv.Handler(),
		}
		inst = &instance{stop: srv.Stop, updateShares: srv.UpdateShares, updateUsers: srv.UpdateUsers}

	case "webdav":
		cfg := valueOr(e.WebDAV)
		e.WebDAV = &cfg
		srv, err := webdavserve.NewServer(&cfg, shares, users)
		if err != nil {
			st.Error = err.Error()
			m.status[e.ID] = st
			slog.Warn("serve server not started", "protocol", e.Protocol, "id", e.ID, "error", err)
			return nil
		}
		b = vhost.Binding{
			Host: cfg.Host, Port: cfg.Port,
			Hostname:   cfg.Hostname,
			TLSCertPEM: cfg.TLSCertPEM, TLSKeyPEM: cfg.TLSKeyPEM,
			Handler: srv.Handler(),
		}
		inst = &instance{updateShares: srv.UpdateShares, updateUsers: srv.UpdateUsers}

	default:
		st.Error = (&unknownProtocolError{protocol: e.Protocol}).Error()
		m.status[e.ID] = st
		return nil
	}

	b.ID = e.ID
	b.Name = e.Name
	if b.Port == 0 {
		b.Port = DefaultPort(e.Protocol)
	}
	inst.sig = entrySignature(e)
	inst.binding = &b
	m.instances[e.ID] = inst
	m.status[e.ID] = st
	return []vhost.Binding{b}
}

// entryHostname returns the configured virtual hostname of a vhost-
// backed entry ("" for everything else).
func entryHostname(e service.ServeServerEntry) string {
	switch e.Protocol {
	case "webdav":
		return valueOr(e.WebDAV).Hostname
	case "s3":
		return valueOr(e.S3).Hostname
	}
	return ""
}

type unknownProtocolError struct{ protocol string }

func (e *unknownProtocolError) Error() string {
	return "unknown serve protocol " + strconv.Quote(e.protocol)
}

// ── helpers ──

// valueOr dereferences a settings pointer, treating nil as protocol
// defaults.
func valueOr[T any](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}

// entrySignature captures the parts of an entry that require a rebind
// when changed. Name and share assignment are excluded: renames are
// cosmetic and share changes are hot-updated.
func entrySignature(e service.ServeServerEntry) string {
	e.Name = ""
	e.Shares = nil
	b, _ := json.Marshal(e)
	return string(b)
}

// filterShares returns the shares an instance exposes: the named subset,
// or everything when no names are given.
func filterShares(all []ftpserve.Share, names []string) []ftpserve.Share {
	if len(names) == 0 {
		return all
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := make([]ftpserve.Share, 0, len(names))
	for _, sh := range all {
		if want[sh.Name] {
			out = append(out, sh)
		}
	}
	return out
}

// entryAddr formats the effective host:port of an entry for display,
// applying protocol defaults.
func entryAddr(e service.ServeServerEntry) string {
	host, port := "", 0
	switch e.Protocol {
	case "ftp":
		c := valueOr(e.FTP)
		host, port = c.Host, c.Port
	case "sftp":
		c := valueOr(e.SFTP)
		host, port = c.Host, c.Port
	case "tftp":
		c := valueOr(e.TFTP)
		host, port = c.Host, c.Port
	case "webdav":
		c := valueOr(e.WebDAV)
		host, port = c.Host, c.Port
	case "s3":
		c := valueOr(e.S3)
		host, port = c.Host, c.Port
	}
	if host == "" {
		host = "0.0.0.0"
	}
	if port == 0 {
		port = DefaultPort(e.Protocol)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// buildShares resolves each share's mount paths to live filesystems.
// A path is "<mount-prefix>" or "<mount-prefix>/<sub/path>". Sources
// whose mount prefix does not resolve are skipped with a warning so a
// single bad reference does not sink the whole share.
func (m *Manager) buildShares(entries []service.FTPShareEntry) []ftpserve.Share {
	out := make([]ftpserve.Share, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		share := ftpserve.Share{Name: e.Name, ReadOnly: e.ReadOnly, Root: e.Root}
		for _, p := range e.Paths {
			prefix, sub := splitMountPath(p)
			if prefix == "" {
				continue
			}
			fs, ok := m.resolver.MountFS(prefix)
			if !ok {
				slog.Warn("serve share source skipped: raw mount not found", "share", e.Name, "mount", prefix)
				continue
			}
			share.Sources = append(share.Sources, ftpserve.ShareSource{Mount: prefix, Path: sub, FS: fs})
		}
		out = append(out, share)
	}
	return out
}

func buildUsers(entries []service.FTPUserEntry) []ftpserve.User {
	out := make([]ftpserve.User, 0, len(entries))
	for _, e := range entries {
		out = append(out, ftpserve.User{
			Username:       e.Username,
			Password:       e.Password,
			Shares:         e.Shares,
			AuthorizedKeys: e.AuthorizedKeys,
			ReadOnly:       e.ReadOnly,
		})
	}
	return out
}

// splitMountPath splits "mount/sub/path" into ("mount", "sub/path").
func splitMountPath(p string) (prefix, sub string) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

// preflightTCP reports whether a TCP listener can bind addr. The probe
// listener is closed immediately; the real server binds a moment later.
func preflightTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// preflightUDP is the TFTP (UDP) equivalent of preflightTCP.
func preflightUDP(addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	return pc.Close()
}
