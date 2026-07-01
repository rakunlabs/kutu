// Package serve wires kutu's built-in file-serving servers (FTP, SFTP,
// TFTP, WebDAV) to the persisted ServeSettings and the live raw-mount
// table. A single Manager owns the four servers; Reconcile is called at
// boot and after every settings / raw-mount mutation to bring the
// running servers in line with the desired configuration.
//
// Shares are resolved against the raw-mount handler on every reconcile,
// so a share that points at "data/releases" serves the live filesystem
// backing the "data" raw mount. Users and shares are shared across the
// protocols (TFTP is anonymous and read-only by protocol design and so
// ignores the user list).
package serve

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
	"github.com/rakunlabs/kutu/internal/serve/sftpserve"
	"github.com/rakunlabs/kutu/internal/serve/tftpserve"
	"github.com/rakunlabs/kutu/internal/serve/webdavserve"
	"github.com/rakunlabs/kutu/internal/service"
)

// Default listen ports, mirrored from the individual server packages so
// the UI status view can show the effective address even before a
// server binds.
const (
	defaultFTPPort    = 2121
	defaultSFTPPort   = 2222
	defaultTFTPPort   = 69
	defaultWebDAVPort = 9119
)

// MountResolver resolves a raw-mount prefix to its live filesystem. The
// api.RawHandler satisfies it; keeping it an interface avoids a server →
// api import cycle and lets shares re-resolve against the hot-reloaded
// mount table on every reconcile.
type MountResolver interface {
	MountFS(prefix string) (rawfs.RawFS, bool)
}

// Status is the runtime state of one serve protocol, surfaced to the UI.
type Status struct {
	Protocol string `json:"protocol"`
	Enabled  bool   `json:"enabled"`
	Running  bool   `json:"running"`
	Address  string `json:"address,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Manager owns the lifecycle of the four serve servers.
type Manager struct {
	mu       sync.Mutex
	appCtx   context.Context
	resolver MountResolver

	// onSFTPKey persists an auto-generated SFTP host key so it survives
	// restarts. May be nil.
	onSFTPKey func(pem string)

	ftp    *ftpserve.Server
	sftp   *sftpserve.Server
	tftp   *tftpserve.Server
	webdav *webdavserve.Server

	ftpCancel    context.CancelFunc
	sftpCancel   context.CancelFunc
	tftpCancel   context.CancelFunc
	webdavCancel context.CancelFunc

	// Last applied bind config per protocol, used to decide between a
	// cheap hot share/user update and a full rebind.
	ftpCfg    service.FTPServeSettings
	sftpCfg   service.SFTPServeSettings
	tftpCfg   service.TFTPServeSettings
	webdavCfg service.WebDAVServeSettings

	status map[string]Status
}

// NewManager constructs a Manager. resolver supplies the raw-mount
// filesystems shares are built from; onSFTPKey (may be nil) persists a
// generated SFTP host key.
func NewManager(appCtx context.Context, resolver MountResolver, onSFTPKey func(pem string)) *Manager {
	return &Manager{
		appCtx:    appCtx,
		resolver:  resolver,
		onSFTPKey: onSFTPKey,
		status:    map[string]Status{},
	}
}

// Reconcile brings the running servers in line with cfg. It is safe to
// call repeatedly; a protocol whose bind config is unchanged keeps its
// listener and only refreshes its shares/users.
func (m *Manager) Reconcile(cfg *service.ServeSettings) {
	if cfg == nil {
		cfg = &service.ServeSettings{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	shares := m.buildShares(cfg.Shares)
	users := buildUsers(cfg.Users)

	m.reconcileFTP(cfg.FTP, shares, users)
	m.reconcileSFTP(cfg.SFTP, shares, users)
	m.reconcileTFTP(cfg.TFTP, shares)
	m.reconcileWebDAV(cfg.WebDAV, shares, users)
}

// Status returns a stable-ordered snapshot of each protocol's runtime
// state.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	order := []string{"ftp", "sftp", "tftp", "webdav"}
	out := make([]Status, 0, len(order))
	for _, p := range order {
		if st, ok := m.status[p]; ok {
			out = append(out, st)
		} else {
			out = append(out, Status{Protocol: p})
		}
	}
	return out
}

// Stop tears down every running server. Called on shutdown.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopFTP()
	m.stopSFTP()
	m.stopTFTP()
	m.stopWebDAV()
}

// ── FTP ──

func (m *Manager) reconcileFTP(cfg service.FTPServeSettings, shares []ftpserve.Share, users []ftpserve.User) {
	st := Status{Protocol: "ftp", Enabled: cfg.Enabled, Address: tcpAddr(cfg.Host, cfg.Port, defaultFTPPort)}

	if m.ftp != nil && m.ftpCfg == cfg && cfg.Enabled {
		m.ftp.UpdateShares(shares)
		m.ftp.UpdateUsers(users)
		st.Running = true
		m.status["ftp"] = st
		return
	}

	m.stopFTP()
	m.ftpCfg = cfg
	if !cfg.Enabled {
		m.status["ftp"] = st
		return
	}
	if err := preflightTCP(st.Address); err != nil {
		st.Error = err.Error()
		m.status["ftp"] = st
		slog.Warn("FTP server not started", "address", st.Address, "error", err)
		return
	}

	srv, err := ftpserve.NewServer(&cfg, shares, users)
	if err != nil {
		st.Error = err.Error()
		m.status["ftp"] = st
		return
	}
	ctx, cancel := context.WithCancel(m.appCtx)
	srv.Start(ctx)
	m.ftp = srv
	m.ftpCancel = cancel
	st.Running = true
	m.status["ftp"] = st
}

func (m *Manager) stopFTP() {
	if m.ftpCancel != nil {
		m.ftpCancel()
		m.ftpCancel = nil
	}
	if m.ftp != nil {
		m.ftp.Stop()
		m.ftp = nil
	}
}

// ── SFTP ──

func (m *Manager) reconcileSFTP(cfg service.SFTPServeSettings, shares []ftpserve.Share, users []ftpserve.User) {
	st := Status{Protocol: "sftp", Enabled: cfg.Enabled, Address: tcpAddr(cfg.Host, cfg.Port, defaultSFTPPort)}

	if m.sftp != nil && m.sftpCfg == cfg && cfg.Enabled {
		m.sftp.UpdateShares(shares)
		m.sftp.UpdateUsers(users)
		st.Running = true
		m.status["sftp"] = st
		return
	}

	m.stopSFTP()
	m.sftpCfg = cfg
	if !cfg.Enabled {
		m.status["sftp"] = st
		return
	}

	// NewServer binds the listener and may generate an ephemeral host
	// key. Persist a generated key and remember it on the applied config
	// so the next reconcile does not see a diff and needlessly rebind.
	onKey := func(pem string) {
		m.sftpCfg.HostKeyPEM = pem
		if m.onSFTPKey != nil {
			m.onSFTPKey(pem)
		}
	}
	srv, err := sftpserve.NewServer(&cfg, shares, users, onKey)
	if err != nil {
		st.Error = err.Error()
		m.status["sftp"] = st
		slog.Warn("SFTP server not started", "address", st.Address, "error", err)
		return
	}
	ctx, cancel := context.WithCancel(m.appCtx)
	srv.Start(ctx)
	m.sftp = srv
	m.sftpCancel = cancel
	st.Running = true
	m.status["sftp"] = st
}

func (m *Manager) stopSFTP() {
	if m.sftpCancel != nil {
		m.sftpCancel()
		m.sftpCancel = nil
	}
	if m.sftp != nil {
		m.sftp.Stop()
		m.sftp = nil
	}
}

// ── TFTP ──

func (m *Manager) reconcileTFTP(cfg service.TFTPServeSettings, shares []ftpserve.Share) {
	st := Status{Protocol: "tftp", Enabled: cfg.Enabled, Address: tcpAddr(cfg.Host, cfg.Port, defaultTFTPPort)}

	if m.tftp != nil && m.tftpCfg == cfg && cfg.Enabled {
		m.tftp.UpdateShares(shares)
		st.Running = true
		m.status["tftp"] = st
		return
	}

	m.stopTFTP()
	m.tftpCfg = cfg
	if !cfg.Enabled {
		m.status["tftp"] = st
		return
	}
	if err := preflightUDP(st.Address); err != nil {
		st.Error = err.Error()
		m.status["tftp"] = st
		slog.Warn("TFTP server not started", "address", st.Address, "error", err)
		return
	}

	srv, err := tftpserve.NewServer(&cfg, shares)
	if err != nil {
		st.Error = err.Error()
		m.status["tftp"] = st
		return
	}
	ctx, cancel := context.WithCancel(m.appCtx)
	srv.Start(ctx, &cfg)
	m.tftp = srv
	m.tftpCancel = cancel
	st.Running = true
	m.status["tftp"] = st
}

func (m *Manager) stopTFTP() {
	if m.tftpCancel != nil {
		m.tftpCancel()
		m.tftpCancel = nil
	}
	if m.tftp != nil {
		m.tftp.Stop()
		m.tftp = nil
	}
}

// ── WebDAV ──

func (m *Manager) reconcileWebDAV(cfg service.WebDAVServeSettings, shares []ftpserve.Share, users []ftpserve.User) {
	st := Status{Protocol: "webdav", Enabled: cfg.Enabled, Address: tcpAddr(cfg.Host, cfg.Port, defaultWebDAVPort)}

	if m.webdav != nil && m.webdavCfg == cfg && cfg.Enabled {
		m.webdav.UpdateShares(shares)
		m.webdav.UpdateUsers(users)
		st.Running = true
		m.status["webdav"] = st
		return
	}

	m.stopWebDAV()
	m.webdavCfg = cfg
	if !cfg.Enabled {
		m.status["webdav"] = st
		return
	}
	if err := preflightTCP(st.Address); err != nil {
		st.Error = err.Error()
		m.status["webdav"] = st
		slog.Warn("WebDAV server not started", "address", st.Address, "error", err)
		return
	}

	srv, err := webdavserve.NewServer(&cfg, shares, users)
	if err != nil {
		st.Error = err.Error()
		m.status["webdav"] = st
		return
	}
	ctx, cancel := context.WithCancel(m.appCtx)
	srv.Start(ctx)
	m.webdav = srv
	m.webdavCancel = cancel
	st.Running = true
	m.status["webdav"] = st
}

func (m *Manager) stopWebDAV() {
	if m.webdavCancel != nil {
		m.webdavCancel()
		m.webdavCancel = nil
	}
	if m.webdav != nil {
		m.webdav.Stop()
		m.webdav = nil
	}
}

// ── helpers ──

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

// tcpAddr formats host:port for display, applying defaults.
func tcpAddr(host string, port, defPort int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	if port == 0 {
		port = defPort
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
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
