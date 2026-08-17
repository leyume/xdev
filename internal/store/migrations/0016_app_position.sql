-- An explicit order for a project's apps, so the project page can be arranged
-- by hand rather than only by creation order.
--
-- Backfilled from the id, which is exactly the order the page already showed
-- (ListAppsByProject ordered by id). Leaving every row at the 0 default would
-- instead hand SQLite a free choice of order within each project and reshuffle
-- pages nobody asked to change.
--
-- The column is per-project rather than global: positions are only ever
-- compared among apps sharing a project_id, so gaps and duplicates across
-- projects are meaningless rather than wrong. Ordering still falls back to id,
-- which keeps the list stable if two rows in one project ever tie.
ALTER TABLE apps ADD COLUMN position INTEGER NOT NULL DEFAULT 0;

UPDATE apps SET position = id;
