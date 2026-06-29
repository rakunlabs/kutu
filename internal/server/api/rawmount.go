package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/service"
)

// Raw mount management layer. Per-entity CRUD on the kutu_raw_mount
// table backing the Settings → Raw mounts UI. Every mutation hot-
// reloads the live mount table via RawHandler.ReloadFromMounts so a
// newly added mount is browsable / serveable without a restart — the
// same reconcile-after-write contract the proxy CRUD uses.
//
// The read side is split in two on purpose:
//
//   - listRawMounts (GET /api/v1/raw-mounts) returns the RUNTIME
//     summary (prefix, type, writable). It only lists mounts that
//     successfully materialized at boot/reload, and is what the file
//     browser + the registry mount pickers consume.
//   - listRawMountConfigs (GET /api/v1/raw-mounts/configs) returns the
//     full PERSISTED config (including backend credentials) for every
//     row regardless of whether its backend is currently reachable, so
//     the management UI can render + edit mounts even when, say, an
//     SFTP host is temporarily down.

// reloadRawMounts rebuilds the live mount table from persisted config.
// Best-effort: a load error leaves the previous table in place rather
// than wiping working mounts.
func (a *api) reloadRawMounts(ctx context.Context) {
	if a.rawHandler == nil {
		return
	}
	mounts, err := a.svc.RawMounts(ctx)
	if err != nil {
		return
	}
	a.rawHandler.ReloadFromMounts(mounts)
}

// listRawMountConfigs returns the full persisted config for every raw
// mount so the management UI can populate its editor.
func (a *api) listRawMountConfigs(c *ada.Context) error {
	mounts, err := a.svc.RawMounts(c.Request.Context())
	if err != nil {
		return err
	}
	if mounts == nil {
		mounts = []service.RawMountEntry{}
	}
	return c.SetStatus(http.StatusOK).SendJSON(mounts)
}

// createRawMount validates and persists a new raw mount, then hot-
// reloads the live table. Returns the stored record so the SPA can
// switch its selection to it immediately.
func (a *api) createRawMount(c *ada.Context) error {
	var m service.RawMountEntry
	if err := c.Bind(&m); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	m.Prefix = strings.TrimSpace(m.Prefix)
	if err := validateRawMount(&m); err != nil {
		return err
	}
	if err := a.svc.CreateRawMount(c.Request.Context(), &m); err != nil {
		return err
	}
	a.reloadRawMounts(c.Request.Context())
	return c.SetStatus(http.StatusCreated).SendJSON(m)
}

// updateRawMount replaces an existing mount. The prefix is taken from
// the path so the body can't rewrite a different row by typo.
func (a *api) updateRawMount(c *ada.Context) error {
	var m service.RawMountEntry
	if err := c.Bind(&m); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	m.Prefix = c.Request.PathValue("prefix")
	if err := validateRawMount(&m); err != nil {
		return err
	}
	if err := a.svc.UpdateRawMount(c.Request.Context(), &m); err != nil {
		return err
	}
	a.reloadRawMounts(c.Request.Context())
	return c.SetStatus(http.StatusOK).SendJSON(m)
}

// deleteRawMount drops the entry and reloads the live table.
func (a *api) deleteRawMount(c *ada.Context) error {
	if err := a.svc.DeleteRawMount(c.Request.Context(), c.Request.PathValue("prefix")); err != nil {
		return err
	}
	a.reloadRawMounts(c.Request.Context())
	return c.SendNoContent()
}

// validateRawMount rejects a malformed entry before it is persisted.
//
// The prefix must be a single path segment (it becomes the first
// component of /raw/<prefix>/...). Per-type required fields are checked
// so the operator gets a precise 400 instead of a mount that silently
// fails to materialize on the next reload.
//
// For local mounts we go one step further and actually construct the
// backend (service.NewRawFS), which stats the directory — a cheap,
// network-free check that surfaces "path does not exist" at save time.
// Remote backends (s3/ftp/sftp/webdav/vercel-blob) are NOT constructed
// here: their constructors open a live connection that the read-only
// RawFS interface gives us no handle to close, so eager validation
// would both leak that connection and make saving impossible whenever
// the remote is momentarily unreachable.
func validateRawMount(m *service.RawMountEntry) error {
	if m.Prefix == "" {
		return errors.Join(errors.New("prefix is required"), service.ErrBadRequest)
	}
	if strings.ContainsAny(m.Prefix, "/\\ ") {
		return errors.Join(errors.New("prefix must be a single path segment (no slashes or spaces)"), service.ErrBadRequest)
	}

	typ := m.Type
	if typ == "" {
		typ = "local"
	}

	switch typ {
	case "local":
		if m.Path == "" {
			return errBadRequest("path is required for a local mount")
		}
		if _, err := service.NewRawFS(*m); err != nil {
			return errors.Join(err, service.ErrBadRequest)
		}
	case "s3":
		if m.S3 == nil || m.S3.Bucket == "" {
			return errBadRequest("s3.bucket is required")
		}
	case "ftp":
		if m.FTP == nil || m.FTP.Host == "" {
			return errBadRequest("ftp.host is required")
		}
	case "sftp":
		if m.SFTP == nil || m.SFTP.Host == "" {
			return errBadRequest("sftp.host is required")
		}
	case "webdav":
		if m.WebDAV == nil || m.WebDAV.URL == "" {
			return errBadRequest("webdav.url is required")
		}
	case "vercel-blob":
		if m.VercelBlob == nil || m.VercelBlob.Token == "" {
			return errBadRequest("vercelBlob.token is required")
		}
	default:
		return errBadRequest(fmt.Sprintf("unknown mount type %q", typ))
	}

	m.Type = typ
	return nil
}

func errBadRequest(msg string) error {
	return errors.Join(errors.New(msg), service.ErrBadRequest)
}
