-- An explicit order for the projects overview, so the cards can be arranged by
-- hand rather than only by age.
--
-- Backfilled to reproduce exactly what the page already showed — newest first
-- (ListProjects ordered by created_at DESC, id DESC) — by counting how many
-- projects sort ahead of each row under those same rules. Leaving every row at
-- the 0 default would instead hand SQLite a free choice within the tie and
-- reshuffle a page nobody asked to change.
--
-- Position is a relative ordering, not an index: only its sign against other
-- rows matters. A new project is given one *below* the current minimum so it
-- keeps arriving at the front, which is where projects have always appeared.
-- Saving an order renumbers the whole list from 1, so those values are tidied
-- up the first time anything is dragged.
ALTER TABLE projects ADD COLUMN position INTEGER NOT NULL DEFAULT 0;

UPDATE projects SET position = 1 + (
  SELECT COUNT(*) FROM projects AS ahead
  WHERE ahead.created_at > projects.created_at
     OR (ahead.created_at = projects.created_at AND ahead.id > projects.id)
);
