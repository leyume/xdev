-- Resource time series for database containers, sampled by the collector from
-- the same `stats` output as app metrics.
--
-- Keyed by container name rather than app id, because the shared MariaDB
-- (xdev-db) is platform infrastructure and has no row in apps. Dedicated
-- per-app DB containers appear here too, deliberately double-counted: they are
-- also part of their app's compose stack and so are already included in that
-- app's row in metrics. The two tables answer different questions ("what is
-- this app costing me" vs "what are my databases costing me"), and a database
-- that is invisible in the second because it belongs to an app would defeat
-- the point of the table.
CREATE TABLE db_metrics (
    container TEXT    NOT NULL,
    ts        TEXT    NOT NULL,
    cpu_pct   REAL    NOT NULL DEFAULT 0,
    mem_bytes INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_db_metrics_container_ts ON db_metrics(container, ts);
