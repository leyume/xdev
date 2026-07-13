-- Shared-database mode (PLANx §B): wordpress/laravel apps can use the single
-- xdev-managed MariaDB server (container xdev-db on the external xdev_shared
-- network) instead of running a per-app db service. The server's root password
-- lives in the settings table (key 'shared_db_root_password'), generated once.
--
--   db_mode  ''        -> dedicated per-app db / not applicable (pre-existing apps)
--            'shared'  -> app's database+user on xdev-db, named <project>_<app>
ALTER TABLE apps ADD COLUMN db_mode TEXT NOT NULL DEFAULT '';
