package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/ada"
	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
	"github.com/rakunlabs/kutu/internal/serve/sftpserve"
	"github.com/rakunlabs/kutu/internal/serve/tftpserve"
	"github.com/rakunlabs/kutu/internal/serve/webdavserve"
	"github.com/rakunlabs/kutu/internal/service"
)

// mountEntry holds a prefix and its associated filesystem backend.
type mountEntry struct {
	Prefix   string
	FS       rawfs.RawFS
	Type     string // "local", "s3", "ftp"
	Writable bool
}

// RawHandler holds the mounted filesystem backends.
// Mounts can be updated at runtime (hot-reload from settings).
type RawHandler struct {
	mu           sync.RWMutex
	mounts       []mountEntry
	ftpServer    *ftpserve.Server
	sftpServer   *sftpserve.Server
	tftpServer   *tftpserve.Server
	webdavServer *webdavserve.Server
	ftpCancel    context.CancelFunc
	sftpCancel   context.CancelFunc
	tftpCancel   context.CancelFunc
	webdavCancel context.CancelFunc
	appCtx       context.Context
	dispatcher   *hook.Dispatcher
}

// NewRawHandler creates a new RawHandler with initial mount entries.
// If dispatcher is non-nil, all mount FS backends are wrapped with hook event emission.
func NewRawHandler(entries []mountEntry, appCtx context.Context, dispatcher *hook.Dispatcher) *RawHandler {
	rh := &RawHandler{appCtx: appCtx, dispatcher: dispatcher}
	rh.mounts = rh.wrapMounts(entries)
	return rh
}

// MountInfo holds serializable info about a mount for the API.
type MountInfo struct {
	Prefix   string `json:"prefix"`
	Type     string `json:"type"`
	Writable bool   `json:"writable"`
}

// MountsInfo returns info about all current mounts.
func (h *RawHandler) MountsInfo() []MountInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]MountInfo, len(h.mounts))
	for i, m := range h.mounts {
		out[i] = MountInfo{
			Prefix:   m.Prefix,
			Type:     m.Type,
			Writable: m.Writable,
		}
	}
	return out
}

// wrapMounts wraps each mount's FS with the hook dispatcher (if set).
func (h *RawHandler) wrapMounts(entries []mountEntry) []mountEntry {
	if h.dispatcher == nil {
		return entries
	}
	wrapped := make([]mountEntry, len(entries))
	for i, m := range entries {
		wrapped[i] = mountEntry{
			Prefix:   m.Prefix,
			FS:       hook.NewHookedFS(m.FS, h.dispatcher, m.Prefix, "http"),
			Type:     m.Type,
			Writable: m.Writable,
		}
	}
	return wrapped
}

// UpdateMounts replaces the current mounts.
func (h *RawHandler) UpdateMounts(entries []mountEntry) {
	wrapped := h.wrapMounts(entries)

	h.mu.Lock()
	h.mounts = wrapped
	h.mu.Unlock()

	for _, m := range entries {
		slog.Info("raw mount updated", "prefix", "/raw/"+m.Prefix, "type", m.Type, "writable", m.Writable)
	}
}

// MountFS returns the rawfs.RawFS backing the named mount prefix.
// Used by callers outside this package (e.g. the registry runtime
// in internal/registry) that need to wrap a raw mount with their
// own abstraction (blobstore, npm metadata cache, ...). Returns
// ok=false when no mount with that prefix exists.
//
// The returned filesystem is the live, hot-reload-tracked instance:
// a subsequent UpdateMounts swap replaces it, so callers must re-
// fetch on settings change rather than caching the handle across
// reloads. (The same pattern as hook/resolve.go which queries the
// mount table on every Resolve call.)
func (h *RawHandler) MountFS(prefix string) (rawfs.RawFS, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := range h.mounts {
		if h.mounts[i].Prefix == prefix {
			return h.mounts[i].FS, true
		}
	}
	return nil, false
}

// resolveMount finds the matching mount for the given request path.
func (h *RawHandler) resolveMount(path string) (*mountEntry, string, error) {
	prefix := path
	rest := ""
	if idx := strings.IndexByte(path, '/'); idx >= 0 {
		prefix = path[:idx]
		rest = path[idx+1:]
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for i := range h.mounts {
		if h.mounts[i].Prefix == prefix {
			return &h.mounts[i], rest, nil
		}
	}

	return nil, "", fmt.Errorf("no mount found for prefix %q: %w", prefix, service.ErrNotFound)
}

// serveRaw handles a raw file read request.
func (h *RawHandler) serveRaw(c *ada.Context) error {
	path := c.Request.PathValue("*")

	mount, rest, err := h.resolveMount(path)
	if err != nil {
		return err
	}

	info, err := mount.FS.Stat(rest)
	if err != nil {
		return mapFSError(err)
	}

	if info.IsDir {
		return h.serveDirectory(c, mount.FS, rest)
	}

	return h.serveFile(c, mount.FS, rest)
}

// serveDirectory returns a JSON listing of directory contents.
func (h *RawHandler) serveDirectory(c *ada.Context, fs rawfs.RawFS, path string) error {
	entries, err := fs.ReadDir(path)
	if err != nil {
		return mapFSError(err)
	}

	c.Response.Header().Set("Content-Type", "application/json")
	c.SetStatus(http.StatusOK)
	return json.NewEncoder(c.Response).Encode(entries)
}

// serveFile serves a single file with Range request support.
func (h *RawHandler) serveFile(c *ada.Context, fs rawfs.RawFS, path string) error {
	reader, info, err := fs.Open(path)
	if err != nil {
		return mapFSError(err)
	}
	defer reader.Close()

	// Detect Content-Type from extension first
	ext := filepath.Ext(info.Name)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		// Fallback: read first 512 bytes for detection
		buf := make([]byte, 512)
		n, _ := reader.Read(buf)
		contentType = http.DetectContentType(buf[:n])
		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seeking file: %w", err)
		}
	}
	c.Response.Header().Set("Content-Type", contentType)

	modTime := info.ModTime
	if modTime.IsZero() {
		modTime = time.Now()
	}

	// ServeContent handles Range, Content-Length, Last-Modified, etc.
	http.ServeContent(c.Response, c.Request, info.Name, modTime, reader)
	return nil
}

// writeFile handles a PUT request to create/overwrite a file.
func (h *RawHandler) writeFile(c *ada.Context) error {
	path := c.Request.PathValue("*")

	mount, rest, err := h.resolveMount(path)
	if err != nil {
		return err
	}

	wfs, ok := mount.FS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("mount %q is read-only: %w", mount.Prefix, service.ErrBadRequest)
	}

	if err := wfs.Write(rest, c.Request.Body, c.Request.ContentLength); err != nil {
		return mapFSError(err)
	}

	c.SetStatus(http.StatusNoContent)
	return nil
}

// deleteFile handles a DELETE request to remove a file.
func (h *RawHandler) deleteFile(c *ada.Context) error {
	path := c.Request.PathValue("*")

	mount, rest, err := h.resolveMount(path)
	if err != nil {
		return err
	}

	wfs, ok := mount.FS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("mount %q is read-only: %w", mount.Prefix, service.ErrBadRequest)
	}

	if err := wfs.Delete(rest); err != nil {
		return mapFSError(err)
	}

	c.SetStatus(http.StatusNoContent)
	return nil
}

// mkDir handles a POST request to create a directory.
func (h *RawHandler) mkDir(c *ada.Context) error {
	path := c.Request.PathValue("*")

	mount, rest, err := h.resolveMount(path)
	if err != nil {
		return err
	}

	wfs, ok := mount.FS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("mount %q is read-only: %w", mount.Prefix, service.ErrBadRequest)
	}

	if err := wfs.MkDir(rest); err != nil {
		return mapFSError(err)
	}

	c.SetStatus(http.StatusNoContent)
	return nil
}

// fileOpRequest is the JSON body for rename/copy/move operations.
type fileOpRequest struct {
	Src string `json:"src"` // "mount/path/to/source"
	Dst string `json:"dst"` // "mount/path/to/destination"
}

// renameFile handles a POST request to rename/move a file within a mount.
func (h *RawHandler) renameFile(c *ada.Context) error {
	var req fileOpRequest
	if err := c.Bind(&req); err != nil {
		return fmt.Errorf("invalid request: %w", service.ErrBadRequest)
	}

	srcMount, srcRest, err := h.resolveMount(req.Src)
	if err != nil {
		return err
	}
	dstMount, dstRest, err := h.resolveMount(req.Dst)
	if err != nil {
		return err
	}

	// Same mount: try native rename
	if srcMount.Prefix == dstMount.Prefix {
		if rfs, ok := srcMount.FS.(rawfs.RenamableRawFS); ok {
			if err := rfs.Rename(srcRest, dstRest); err != nil {
				return mapFSError(err)
			}
			c.SetStatus(http.StatusNoContent)
			return nil
		}
	}

	// Cross-mount or no native rename: copy + delete
	dstWFS, ok := dstMount.FS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("destination mount %q is read-only: %w", dstMount.Prefix, service.ErrBadRequest)
	}
	srcWFS, ok2 := srcMount.FS.(rawfs.WritableRawFS)
	if !ok2 {
		return fmt.Errorf("source mount %q is read-only (cannot delete after copy): %w", srcMount.Prefix, service.ErrBadRequest)
	}

	if err := rawfs.GenericCopy(srcMount.FS, srcRest, dstWFS, dstRest); err != nil {
		return mapFSError(err)
	}
	if err := srcWFS.Delete(srcRest); err != nil {
		return mapFSError(err)
	}

	c.SetStatus(http.StatusNoContent)
	return nil
}

// copyFile handles a POST request to copy a file.
func (h *RawHandler) copyFile(c *ada.Context) error {
	var req fileOpRequest
	if err := c.Bind(&req); err != nil {
		return fmt.Errorf("invalid request: %w", service.ErrBadRequest)
	}

	srcMount, srcRest, err := h.resolveMount(req.Src)
	if err != nil {
		return err
	}
	dstMount, dstRest, err := h.resolveMount(req.Dst)
	if err != nil {
		return err
	}

	// Same mount: try native copy
	if srcMount.Prefix == dstMount.Prefix {
		if cfs, ok := srcMount.FS.(rawfs.CopyableRawFS); ok {
			if err := cfs.Copy(srcRest, dstRest); err != nil {
				return mapFSError(err)
			}
			c.SetStatus(http.StatusNoContent)
			return nil
		}
	}

	// Fallback: generic copy
	dstWFS, ok := dstMount.FS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("destination mount %q is read-only: %w", dstMount.Prefix, service.ErrBadRequest)
	}

	if err := rawfs.GenericCopy(srcMount.FS, srcRest, dstWFS, dstRest); err != nil {
		return mapFSError(err)
	}

	c.SetStatus(http.StatusNoContent)
	return nil
}

// moveFile handles a POST request to move a file (copy + delete).
func (h *RawHandler) moveFile(c *ada.Context) error {
	return h.renameFile(c) // move is the same as rename
}

// mapFSError converts filesystem errors to service errors for proper HTTP status codes.
func mapFSError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%v: %w", err, service.ErrNotFound)
	}
	return err
}

// getRaw serves a raw file or directory listing. Accepts an API token
// (Authorization: Bearer / Basic), Basic username+password, or the UI
// session cookie. Listing a directory is allowed on ancestors of a
// permitted path so scoped users can navigate down to it.
func (a *api) getRaw(c *ada.Context) error {
	path := c.Request.PathValue("*")
	p, err := a.authorizeRaw(c, service.CapRawRead, path, true)
	if err != nil {
		return a.rawAuthError(c, err)
	}
	c.Request = withPrincipal(c.Request, p)
	return a.rawHandler.serveRaw(c)
}

// putRaw uploads a file.
func (a *api) putRaw(c *ada.Context) error {
	p, err := a.authorizeRaw(c, service.CapRawWrite, c.Request.PathValue("*"), false)
	if err != nil {
		return a.rawAuthError(c, err)
	}
	c.Request = withPrincipal(c.Request, p)
	return a.rawHandler.writeFile(c)
}

// deleteRaw deletes a file. raw.write covers deletes.
func (a *api) deleteRaw(c *ada.Context) error {
	p, err := a.authorizeRaw(c, service.CapRawWrite, c.Request.PathValue("*"), false)
	if err != nil {
		return a.rawAuthError(c, err)
	}
	c.Request = withPrincipal(c.Request, p)
	return a.rawHandler.deleteFile(c)
}

// rawAuthError adds a Basic challenge to anonymous raw requests so curl
// and download tools know to send credentials. Browsers are not prompted:
// the challenge is only sent when the request is not from the SPA.
func (a *api) rawAuthError(c *ada.Context, err error) error {
	if errors.Is(err, errNoCredentials) && c.Request.Header.Get("X-Requested-With") == "" &&
		!strings.Contains(c.Request.Header.Get("Accept"), "application/json") {
		c.Response.Header().Set("WWW-Authenticate", `Basic realm="kutu"`)
	}
	return err
}

// withRawFileOp guards rename/copy/move: read on the source and write on
// the destination (plus write on the source for moves).
func (a *api) withRawFileOp(move bool, handler func(*ada.Context) error) func(*ada.Context) error {
	return func(c *ada.Context) error {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<16))
		if err != nil {
			return errors.Join(err, service.ErrBadRequest)
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		var req fileOpRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("invalid request: %w", service.ErrBadRequest)
		}
		ctx := c.Request.Context()
		caps := service.CapabilitiesFromContext(ctx)
		pats := service.CapabilityPatternsFromContext(ctx)
		check := func(capKey, path string) error {
			if !caps.Has(capKey) || !pats.Allows(capKey, path) {
				return fmt.Errorf("path %q not permitted for %q: %w", path, capKey, service.ErrForbidden)
			}
			return nil
		}
		if err := check(service.CapRawRead, req.Src); err != nil {
			return err
		}
		if move {
			if err := check(service.CapRawWrite, req.Src); err != nil {
				return err
			}
		}
		if err := check(service.CapRawWrite, req.Dst); err != nil {
			return err
		}
		return handler(c)
	}
}

// Dispatcher returns the hook dispatcher (may be nil if no mounts are configured).
func (h *RawHandler) Dispatcher() *hook.Dispatcher {
	if h == nil {
		return nil
	}
	return h.dispatcher
}
