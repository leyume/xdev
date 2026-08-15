-- A host app (static, go) can take its code from a git repository instead of a
-- folder xdev scaffolds or one the user points it at. xdev clones the repo into
-- the app's managed directory, runs the build command there, and serves the
-- build output — so a deploy is "fetch, reset, build" rather than a file copy.
--
--   git_url      ''        -> not a git app (scaffold / source_dir, as before)
--                'git@github.com:owner/repo.git' | 'https://github.com/owner/repo'
--   git_ref      branch or tag to track ('' = the remote's default branch)
--   git_subdir   build from a subdirectory of the repo ('' = the repo root),
--                for a monorepo whose frontend lives in e.g. apps/web
--   deployed_sha the commit currently built and served ('' = never deployed)
--   deployed_at  when that commit was deployed
--
-- git_url and source_dir are mutually exclusive: a deploy runs `git reset
-- --hard`, which xdev must never do to a directory the user owns.
ALTER TABLE apps ADD COLUMN git_url      TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN git_ref      TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN git_subdir   TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN deployed_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN deployed_at  TEXT NOT NULL DEFAULT '';

-- An SSH deploy key per app, for private repositories. GitHub allows a given
-- deploy key on exactly one repository per account, so these cannot be shared
-- between apps even if they point at the same owner — one app, one key.
--
-- The private half is encrypted at rest (AES-256-GCM under the key file at
-- <data-dir>/secret.key) so a copy of the database — a backup, a scp, an
-- accidental commit — does not hand over repository access on its own. It is
-- not protection against someone who is already root on this host: the key file
-- sits beside the database and that user can read both.
--
-- app_id is 0 until the key is bound to an app. The add-app form generates the
-- key first so the public half can be pasted into GitHub *before* the first
-- clone is attempted, which is the only order that works for a private repo.
-- Unbound keys are swept on startup.
CREATE TABLE deploy_keys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id      INTEGER NOT NULL DEFAULT 0,
    public_key  TEXT    NOT NULL,          -- 'ssh-ed25519 AAAA... xdev-<slug>'
    private_key TEXT    NOT NULL,          -- base64(nonce || AES-GCM ciphertext)
    fingerprint TEXT    NOT NULL,          -- 'SHA256:...' for display
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_deploy_keys_app ON deploy_keys(app_id);
