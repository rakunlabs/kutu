-- Users, permission bundles, sessions, API tokens and second factors.
--
-- Users are created through the first-user bootstrap (signup-first) or by
-- an admin; external identities (OAuth2 / header) link to a user row.

CREATE TABLE IF NOT EXISTS kutu_user (
    id                  TEXT PRIMARY KEY,
    username            TEXT        NOT NULL,
    email               TEXT        NOT NULL DEFAULT '',
    display_name        TEXT        NOT NULL DEFAULT '',
    -- bcrypt hash; empty for external-only users.
    password_hash       TEXT        NOT NULL DEFAULT '',
    external            BOOLEAN     NOT NULL DEFAULT false,
    disabled            BOOLEAN     NOT NULL DEFAULT false,
    is_superadmin       BOOLEAN     NOT NULL DEFAULT false,
    -- Per-user deny overlay: capability keys subtracted from every grant
    -- source except the operator superadmin allowlist.
    denied_capabilities JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS kutu_user_username_uq ON kutu_user (username);
CREATE INDEX IF NOT EXISTS kutu_user_email_idx ON kutu_user (email) WHERE email <> '';

CREATE TABLE IF NOT EXISTS kutu_user_identity (
    id            TEXT PRIMARY KEY,
    user_id       TEXT        NOT NULL REFERENCES kutu_user(id) ON DELETE CASCADE,
    provider      TEXT        NOT NULL,
    subject       TEXT        NOT NULL,
    email         TEXT        NOT NULL DEFAULT '',
    display_name  TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    UNIQUE (provider, subject)
);

CREATE INDEX IF NOT EXISTS kutu_user_identity_user_idx ON kutu_user_identity (user_id);

-- Permission bundles: a named set of capability keys, each optionally
-- narrowed to doublestar path patterns (key_patterns: {"raw.read": ["builds/**"]}).
CREATE TABLE IF NOT EXISTS kutu_permission (
    id           TEXT PRIMARY KEY,
    key          TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    keys         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    key_patterns JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS kutu_user_permission (
    user_id       TEXT NOT NULL REFERENCES kutu_user(id) ON DELETE CASCADE,
    permission_id TEXT NOT NULL REFERENCES kutu_permission(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, permission_id)
);

CREATE INDEX IF NOT EXISTS kutu_user_permission_perm_idx ON kutu_user_permission (permission_id);

-- Login sessions. id is the raw cookie value; payload is ada's issuer pair.
CREATE TABLE IF NOT EXISTS kutu_session (
    id         TEXT PRIMARY KEY,
    user_id    TEXT        NOT NULL DEFAULT '',
    username   TEXT        NOT NULL DEFAULT '',
    payload    BYTEA,
    refresh_id TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS kutu_session_user_idx ON kutu_session (user_id);
CREATE INDEX IF NOT EXISTS kutu_session_expires_idx ON kutu_session (expires_at);

-- API tokens. Only the SHA-256 of the raw key is stored.
CREATE TABLE IF NOT EXISTS kutu_token (
    id           TEXT PRIMARY KEY,
    name         TEXT        NOT NULL,
    hashed_key   TEXT        NOT NULL UNIQUE,
    scopes       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    active       BOOLEAN     NOT NULL DEFAULT true,
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS kutu_passkey (
    id               TEXT PRIMARY KEY,
    user_id          TEXT        NOT NULL REFERENCES kutu_user(id) ON DELETE CASCADE,
    credential_id    BYTEA       NOT NULL UNIQUE,
    public_key       BYTEA       NOT NULL,
    aaguid           BYTEA,
    sign_count       BIGINT      NOT NULL DEFAULT 0,
    transports       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    user_verified    BOOLEAN     NOT NULL DEFAULT false,
    backup_eligible  BOOLEAN     NOT NULL DEFAULT false,
    backup_state     BOOLEAN     NOT NULL DEFAULT false,
    attestation_type TEXT        NOT NULL DEFAULT '',
    name             TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS kutu_passkey_user_idx ON kutu_passkey (user_id);

CREATE TABLE IF NOT EXISTS kutu_passkey_challenge (
    id         TEXT PRIMARY KEY,
    kind       TEXT        NOT NULL,
    user_id    TEXT        NOT NULL DEFAULT '',
    data       BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS kutu_passkey_challenge_expires_idx ON kutu_passkey_challenge (expires_at);

CREATE TABLE IF NOT EXISTS kutu_user_totp (
    user_id        TEXT PRIMARY KEY REFERENCES kutu_user(id) ON DELETE CASCADE,
    secret         TEXT        NOT NULL,
    enabled        BOOLEAN     NOT NULL DEFAULT false,
    recovery_codes JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ
);
