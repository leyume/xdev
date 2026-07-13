-- Shared-host WordPress (PLANx §C1): a wordpress app can be served by the
-- single platform xdev-wp PHP-FPM container from a docroot under
-- data/wp/sites/<project>_<app>/ instead of running its own container stack.
--
--   wp_mode  ''        -> separate container / not applicable (pre-existing apps)
--            'shared'  -> shared wp-host; no container, compose file, or port
ALTER TABLE apps ADD COLUMN wp_mode TEXT NOT NULL DEFAULT '';
