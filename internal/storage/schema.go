package storage

const schema = `
CREATE TABLE IF NOT EXISTS calls (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp     INTEGER NOT NULL,
    api_key_id    TEXT NOT NULL,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    endpoint      TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cost_usd      REAL NOT NULL,
    latency_ms    INTEGER NOT NULL,
    status        INTEGER NOT NULL,
    error_class   TEXT NOT NULL DEFAULT '',
    prompt_hash   TEXT NOT NULL DEFAULT '',
    session_id    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS calls_ts_idx ON calls(timestamp);
CREATE INDEX IF NOT EXISTS calls_key_ts_idx ON calls(api_key_id, timestamp);

CREATE TABLE IF NOT EXISTS api_keys (
    id            TEXT PRIMARY KEY,
    hashed_key    TEXT NOT NULL,
    label         TEXT NOT NULL DEFAULT '',
    budget_usd    REAL NOT NULL DEFAULT 0,
    rate_limit_rpm INTEGER NOT NULL DEFAULT 0,
    active        INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS anomalies (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    detected_at     INTEGER NOT NULL,
    api_key_id      TEXT NOT NULL,
    date            TEXT NOT NULL,
    metric          TEXT NOT NULL,
    baseline_value  REAL NOT NULL,
    actual_value    REAL NOT NULL,
    delta           REAL NOT NULL,
    sigma           REAL NOT NULL,
    sample_size     INTEGER NOT NULL,
    UNIQUE(api_key_id, date, metric)
);

CREATE TABLE IF NOT EXISTS deploys (
    id            TEXT PRIMARY KEY,
    repo          TEXT NOT NULL,
    branch        TEXT NOT NULL,
    commit_sha    TEXT NOT NULL,
    pr_number     INTEGER,
    title         TEXT NOT NULL,
    started_at    INTEGER NOT NULL,
    completed_at  INTEGER NOT NULL,
    status        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS correlations (
    anomaly_id    INTEGER NOT NULL,
    deploy_id     TEXT NOT NULL,
    confidence    REAL NOT NULL,
    evidence      TEXT NOT NULL,
    PRIMARY KEY (anomaly_id, deploy_id)
);

CREATE TABLE IF NOT EXISTS sync_cursors (
    name  TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`
