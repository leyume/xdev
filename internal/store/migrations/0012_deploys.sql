-- Deploying an app from outside the UI: GitHub tells xdev to pull (a webhook),
-- or CI hands xdev a built site (a push). Both need an endpoint that is
-- reachable from the internet, and neither can use the session cookie the web
-- UI authenticates with, so each carries its own credential.
--
-- The endpoints are served on the app's **own** hostname under /_xdev/ — the
-- domain already routes through Caddy, so there is no second hostname to point
-- at the server, no extra certificate, and no way to reach the admin UI through
-- them: Caddy only proxies those two paths.
--
--   hook_id          path segment of the webhook URL; '' = webhook off
--                    https://<app-domain>/_xdev/hook/<hook_id>
--   hook_secret      HMAC secret shared with GitHub (encrypted at rest), which
--                    signs the body as X-Hub-Signature-256
--   push_token_hash  sha256 of the bearer token CI sends; '' = push off. The
--                    token itself is shown once and never stored — a leaked
--                    database does not hand over the ability to deploy.
--   push_token_hint  a few leading characters, so the UI can say which token is
--                    configured without holding it
ALTER TABLE apps ADD COLUMN hook_id         TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN hook_secret     TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN push_token_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN push_token_hint TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_apps_hook_id ON apps(hook_id) WHERE hook_id <> '';

-- One row per deploy attempt. Deploys are asynchronous — a webhook cannot wait
-- for `npm ci` — so this is where the UI reads what happened: which commit,
-- what triggered it, whether it worked, and the build output when it did not.
--
--   trigger  manual | webhook | push
--   status   running | ok | failed
--   log      build output, trimmed; the reason a failed deploy is diagnosable
CREATE TABLE deployments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    trigger     TEXT    NOT NULL DEFAULT 'manual',
    status      TEXT    NOT NULL DEFAULT 'running',
    sha         TEXT    NOT NULL DEFAULT '',
    message     TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    finished_at TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_deployments_app ON deployments(app_id, id DESC);
