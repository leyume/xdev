-- Static apps run on the host's system Node (or are served straight off disk by
-- Caddy) instead of in a container, so they carry their own small config rather
-- than a generated compose file. These columns are blank for container apps.
--
--   serve_mode  'serve'   -> Caddy file-servers root_dir directly (no process)
--               'command' -> xdev supervises start_cmd as a host process on `port`
--   root_dir    served subdir for serve mode ('' = the app folder itself)
--   build_cmd   optional one-shot build step (system Node), run on start/deploy
--   start_cmd   long-lived command for command mode (system Node)
ALTER TABLE apps ADD COLUMN serve_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN root_dir   TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN build_cmd  TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN start_cmd  TEXT NOT NULL DEFAULT '';
