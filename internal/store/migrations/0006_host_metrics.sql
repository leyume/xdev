-- Host-level resource time series (CPU% / memory%), sampled by the collector
-- alongside per-app metrics. Pruned to the same 24h retention.
CREATE TABLE host_metrics (
    ts      TEXT NOT NULL,
    cpu_pct REAL NOT NULL DEFAULT 0,
    mem_pct REAL NOT NULL DEFAULT 0
);
CREATE INDEX idx_host_metrics_ts ON host_metrics(ts);
