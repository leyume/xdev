-- Initial schema for xdev.
--
-- Two-level model: a `project` groups one or more `apps` (components). bizepp
-- is the canonical example: one project = a Laravel backend app + a Vue
-- frontend app, sharing a private network and a base domain.
--
-- Phase 0 only reads/writes users, sessions, settings, and (empty) projects,
-- but the full schema is created up front so later phases need no migration
-- churn.

-- Single admin user in v1; table shaped to grow into multi-user later.
CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    email         TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- Server-side sessions. The cookie carries `token`; everything else stays here.
-- csrf_token is bound to the session and validated on unsafe requests.
CREATE TABLE sessions (
    token      TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token TEXT    NOT NULL,
    expires_at TEXT    NOT NULL,
    created_at TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- Global key/value settings (default engine, base TLD, hosts-mgmt toggle, ...).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- A project: top-level group with its own dir, shared network, and base domain.
CREATE TABLE projects (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    slug         TEXT    NOT NULL UNIQUE,
    base_domain  TEXT    NOT NULL DEFAULT '',   -- e.g. bizepp.test / bizepp.com
    environment  TEXT    NOT NULL DEFAULT 'local', -- local | prod
    network_name TEXT    NOT NULL DEFAULT '',
    dir          TEXT    NOT NULL DEFAULT '',   -- projects/<slug>
    created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- An app (component) inside a project: one deployable compose stack.
CREATE TABLE apps (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    slug         TEXT    NOT NULL,
    type         TEXT    NOT NULL,              -- wordpress | laravel | static-prebuilt | static-build
    runtime      TEXT    NOT NULL DEFAULT '',   -- podman | docker (blank = project/global default)
    status       TEXT    NOT NULL DEFAULT 'stopped', -- stopped | running | error
    subdomain    TEXT    NOT NULL DEFAULT '',   -- e.g. api -> api.bizepp.test
    cpu_limit    REAL    NOT NULL DEFAULT 0,    -- cores; 0 = unlimited
    mem_limit    INTEGER NOT NULL DEFAULT 0,    -- bytes; 0 = unlimited
    port         INTEGER NOT NULL DEFAULT 0,    -- optional host port
    compose_path TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (project_id, slug)
);

-- Per-app environment variables, rendered into the app's .env file.
CREATE TABLE app_env (
    app_id INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    key    TEXT    NOT NULL,
    value  TEXT    NOT NULL,
    PRIMARY KEY (app_id, key)
);

-- Domains attached to an app (local .test or live), plus SSL mode/status.
CREATE TABLE domains (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    hostname    TEXT    NOT NULL UNIQUE,
    is_local    INTEGER NOT NULL DEFAULT 1,        -- 1 = .test/local, 0 = live
    ssl_mode    TEXT    NOT NULL DEFAULT 'internal', -- internal | letsencrypt
    cert_status TEXT    NOT NULL DEFAULT ''
);

-- Time-series resource metrics per app (raw 24h + rollups, pruned by a job).
CREATE TABLE metrics (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id    INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    ts        TEXT    NOT NULL,
    cpu_pct   REAL    NOT NULL DEFAULT 0,
    mem_bytes INTEGER NOT NULL DEFAULT 0,
    mem_limit INTEGER NOT NULL DEFAULT 0,
    net_rx    INTEGER NOT NULL DEFAULT 0,
    net_tx    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_metrics_app_ts ON metrics(app_id, ts);

-- Audit log of meaningful actions.
CREATE TABLE events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    app_id     INTEGER REFERENCES apps(id) ON DELETE SET NULL,
    ts         TEXT    NOT NULL DEFAULT (datetime('now')),
    level      TEXT    NOT NULL DEFAULT 'info',
    message    TEXT    NOT NULL
);
