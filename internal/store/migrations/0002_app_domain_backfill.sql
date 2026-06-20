-- The `subdomain` column on apps used to hold just the subdomain label; it now
-- holds the app's full hostname. Backfill existing rows from the domains table
-- (which already stored the full hostname) so older apps display and edit the
-- correct domain. New installs have no rows to update.
UPDATE apps
SET subdomain = (
    SELECT d.hostname FROM domains d WHERE d.app_id = apps.id LIMIT 1
)
WHERE EXISTS (SELECT 1 FROM domains d WHERE d.app_id = apps.id);
