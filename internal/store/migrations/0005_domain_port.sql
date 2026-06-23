-- Let a domain carry its own upstream host port so one app can route several
-- services (e.g. a Laravel app's Octane server and its Adminer). A port of 0
-- means "use the app's own port" (the primary domain); a non-zero port routes
-- straight to that port (a secondary service like Adminer).
ALTER TABLE domains ADD COLUMN port INTEGER NOT NULL DEFAULT 0;
