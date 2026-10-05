package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/service"
)

func (a *api) healthzHandler(c *ada.Context) error {
	return c.SetStatus(http.StatusOK).SendString("OK")
}

// infoHandler returns server metadata, the caller's identity and
// effective capabilities, the capability vocabulary and the at-rest key
// status. It is public (and on the lockgate allowlist) so the SPA can
// decide between the login, unlock and app screens.
func (a *api) infoHandler(c *ada.Context) error {
	ctx := c.Request.Context()

	initialized, unlocked := false, true
	if st, err := a.svc.GetKeyStatus(ctx); err == nil {
		initialized = st.Initialized
		unlocked = st.Unlocked
	}

	var username, userID, subject string
	caps := []string{}
	if a.mgr != nil {
		if id, keys, user, uid, _ := a.mgr.ResolveRequest(c.Request); id != nil {
			subject = id.Subject
			username, userID = user, uid
			if username == "" {
				username = id.Subject
			}
			if keys != nil {
				caps = keys
			}
		}
	}

	authSettings := a.svc.GetAuthSettings(ctx)
	isSuperadmin := false
	var subtitle, localLoginName string
	localLoginFormCollapsed := false
	if eff := authSettings.WithEffectiveDefaults(); eff != nil {
		subtitle = eff.UI.Subtitle
		if eff.Local != nil && eff.Local.Enabled {
			localLoginName = eff.Local.Name
			if localLoginName == "" {
				localLoginName = "local"
			}
			localLoginFormCollapsed = eff.Local.LoginFormCollapsed
		}
	}
	if authSettings != nil && subject != "" {
		for _, s := range authSettings.Capabilities.Superadmins {
			if s == subject {
				isSuperadmin = true
				break
			}
		}
	}
	if !isSuperadmin && userID != "" {
		if user, err := a.svc.GetUserByID(ctx, userID); err == nil {
			isSuperadmin = user.IsSuperadmin
		}
	}

	setupRequired := false
	if username == "" {
		if n, err := a.svc.UserCount(ctx); err == nil && n == 0 {
			setupRequired = true
		}
	}

	return c.SetStatus(http.StatusOK).SendJSON(struct {
		Info
		Subtitle                 string               `json:"subtitle,omitempty"`
		User                     string               `json:"user,omitempty"`
		UserID                   string               `json:"user_id,omitempty"`
		AuthEnabled              bool                 `json:"auth_enabled"`
		IsSuperadmin             bool                 `json:"is_superadmin"`
		Permissions              []string             `json:"permissions"`
		Capabilities             []service.Capability `json:"capabilities"`
		SetupRequired            bool                 `json:"setup_required,omitempty"`
		LocalLoginName           string               `json:"local_login_name,omitempty"`
		LocalLoginFormCollapsed  bool                 `json:"local_login_form_collapsed"`
		AccountSecurityAvailable bool                 `json:"account_security_available"`
		KeyInitialized           bool                 `json:"key_initialized"`
		KeyUnlocked              bool                 `json:"key_unlocked"`
	}{
		Info:                     a.info,
		Subtitle:                 subtitle,
		User:                     username,
		UserID:                   userID,
		AuthEnabled:              true,
		IsSuperadmin:             isSuperadmin,
		Permissions:              caps,
		Capabilities:             service.KnownCapabilities,
		SetupRequired:            setupRequired,
		LocalLoginName:           localLoginName,
		LocalLoginFormCollapsed:  localLoginFormCollapsed,
		AccountSecurityAvailable: authSettings.AccountSecurityAllowed(isSuperadmin),
		KeyInitialized:           initialized,
		KeyUnlocked:              unlocked,
	})
}

// getAuthSettings returns the effective auth settings with OAuth2 client
// secrets masked. GET /api/v1/settings/auth.
func (a *api) getAuthSettings(c *ada.Context) error {
	s := a.svc.GetAuthSettings(c.Request.Context()).WithEffectiveDefaults()
	s.MaskSecrets()
	return c.SetStatus(http.StatusOK).SendJSON(s)
}

// putAuthSettings validates, persists and hot-reloads the auth settings.
// Cookie, issuer and rate-limit changes apply on the next restart.
// PUT /api/v1/settings/auth.
func (a *api) putAuthSettings(c *ada.Context) error {
	var req service.AuthSettings
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	ctx := c.Request.Context()
	if err := a.svc.SetAuthSettings(ctx, &req); err != nil {
		return err
	}
	stored := a.svc.GetAuthSettings(ctx)
	if err := a.mgr.Reload(ctx, stored); err != nil {
		return fmt.Errorf("auth settings saved but reload failed: %w", err)
	}
	out := a.svc.GetAuthSettings(ctx).WithEffectiveDefaults()
	out.MaskSecrets()
	return c.SetStatus(http.StatusOK).SendJSON(out)
}

// changeMyPassword rotates the caller's local password and revokes their
// other sessions. POST /api/v1/me/password.
func (a *api) changeMyPassword(c *ada.Context) error {
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	ctx := c.Request.Context()
	if err := a.svc.ChangeOwnPassword(ctx, service.UserIDFromContext(ctx),
		req.CurrentPassword, req.NewPassword, a.mgr.CurrentSessionID(c.Request)); err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(response{Message: "password changed"})
}
