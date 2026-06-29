package api

import (
	"errors"
	"net/http"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/service"
)

// HTTP wrappers for the server-key lifecycle. The logic lives in
// service/keyops.go — these handlers only bind requests and map status.
// Registered under /api/v1/key/* in api.go. status/unlock are on the
// lockgate allowlist so the SPA can drive the unlock flow while locked.

// getKeyStatus reports whether a verifier exists (initialized) and
// whether the live key is loaded (unlocked). Cheap, secret-free.
func (a *api) getKeyStatus(c *ada.Context) error {
	st, err := a.svc.GetKeyStatus(c.Request.Context())
	if err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(st)
}

// postKeyInitialize sets the at-rest key for the first time. 409 when a
// verifier already exists.
func (a *api) postKeyInitialize(c *ada.Context) error {
	var req struct {
		Key string `json:"key"`
	}
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	if err := a.svc.InitializeServerKey(c.Request.Context(), req.Key); err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(response{Message: "server initialized and unlocked"})
}

// postKeyUnlock loads the supplied key after validating it against the
// stored verifier. Wrong key → 403.
func (a *api) postKeyUnlock(c *ada.Context) error {
	var req struct {
		Key string `json:"key"`
	}
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	if err := a.svc.UnlockServerKey(c.Request.Context(), req.Key); err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(response{Message: "server unlocked"})
}

// postKeyLock manually clears the live key.
func (a *api) postKeyLock(c *ada.Context) error {
	if err := a.svc.LockServerKey(); err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(response{Message: "server locked"})
}

// postKeyRotate validates the old key, rewraps the verifier and every
// sealed secret with the new key, and swaps the live encryptor.
func (a *api) postKeyRotate(c *ada.Context) error {
	var req struct {
		CurrentKey string `json:"current_key"`
		NewKey     string `json:"new_key"`
	}
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	if err := a.svc.RotateServerKey(c.Request.Context(), req.CurrentKey, req.NewKey); err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(response{Message: "server key rotated"})
}
