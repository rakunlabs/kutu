-- kutu relational config schema.
--
-- One table per config entity. Only the fields used for filtering /
-- sorting (via rakunlabs/query) are real columns; everything else is
-- folded into a `data` JSONB blob. Registry repository credentials are
-- split out: non-secret auth metadata in `auth` JSONB, sealed
-- credential material in `auth_sealed` BYTEA.
--
-- Every table carries `updated_by`, set from the optional X-User
-- request header so changes can be attributed. Registry ARTIFACTS are
-- NOT stored here — they live on raw mounts via the blobstore/rawfs
-- abstraction. These tables hold configuration only.

-- ── Registry ──
CREATE TABLE IF NOT EXISTS kutu_registry_namespace (
    name        TEXT PRIMARY KEY,
    description TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS kutu_registry_repository (
    namespace   TEXT NOT NULL REFERENCES kutu_registry_namespace(name) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    -- Non-secret upstream-auth metadata (type / username / header).
    auth        JSONB,
    -- Sealed credential material {password, token, value}.
    auth_sealed BYTEA,
    -- Everything else (description, mount, base_path, allow_push, url,
    -- mutable_ttl, insecure_skip_verify, default_local, max_upload_size,
    -- members, floating_tags, cors_origins, policy).
    data        JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (namespace, name)
);

CREATE INDEX IF NOT EXISTS kutu_registry_repository_type_idx ON kutu_registry_repository (type);
CREATE INDEX IF NOT EXISTS kutu_registry_repository_kind_idx ON kutu_registry_repository (kind);

-- ── Raw mounts ──
CREATE TABLE IF NOT EXISTS kutu_raw_mount (
    prefix     TEXT PRIMARY KEY,
    type       TEXT        NOT NULL DEFAULT 'local',
    -- Full polymorphic mount config (path / s3 / ftp / sftp / webdav / vercel).
    config     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT        NOT NULL DEFAULT ''
);

-- ── Proxy ──
CREATE TABLE IF NOT EXISTS kutu_proxy_listener (
    id         TEXT PRIMARY KEY,
    name       TEXT        NOT NULL DEFAULT '',
    protocol   TEXT        NOT NULL DEFAULT 'http',
    port       TEXT        NOT NULL DEFAULT '',
    enabled    BOOLEAN     NOT NULL DEFAULT false,
    -- host, tls, notes.
    data       JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS kutu_proxy_server (
    id          TEXT PRIMARY KEY,
    name        TEXT        NOT NULL DEFAULT '',
    enabled     BOOLEAN     NOT NULL DEFAULT false,
    listener_id TEXT        NOT NULL DEFAULT '',
    protocol    TEXT        NOT NULL DEFAULT '',
    -- host, port, host_match, nodes, edges, pipeline.
    data        JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS kutu_proxy_server_listener_idx ON kutu_proxy_server (listener_id);

-- ── Hooks ──
CREATE TABLE IF NOT EXISTS kutu_hook (
    name       TEXT PRIMARY KEY,
    enabled    BOOLEAN     NOT NULL DEFAULT false,
    -- events, filter, targets.
    data       JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT        NOT NULL DEFAULT ''
);

-- ── Singletons (encryption verifier, feature flags) ──
CREATE TABLE IF NOT EXISTS kutu_meta (
    key        TEXT PRIMARY KEY,
    value      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT        NOT NULL DEFAULT ''
);
