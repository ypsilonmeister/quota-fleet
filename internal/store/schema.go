package store

const schemaSQL = `
CREATE TABLE IF NOT EXISTS jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL CHECK(length(trim(title)) > 0),
    status TEXT NOT NULL CHECK(status IN ('draft','queued','claimed','waiting','done','failed','cancelled')),
    agent TEXT NOT NULL DEFAULT '',
    host TEXT NOT NULL DEFAULT '',
    workdir TEXT NOT NULL DEFAULT '',
    worktree_spec TEXT NOT NULL DEFAULT '',
    target_ref TEXT NOT NULL DEFAULT '',
    instructions TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    parent_id INTEGER NULL REFERENCES jobs(id),
    claimed_by TEXT NOT NULL DEFAULT '',
    claimed_at TEXT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    result_text TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    delivered_at TEXT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    pool TEXT NOT NULL DEFAULT '',
    alt_pools TEXT NOT NULL DEFAULT '[]',
    session_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_jobs_status_id ON jobs(status, id);
CREATE INDEX IF NOT EXISTS idx_jobs_parent_id ON jobs(parent_id);
CREATE INDEX IF NOT EXISTS idx_jobs_done_undelivered
    ON jobs(status, delivered_at)
    WHERE status = 'done' AND delivered_at IS NULL;

CREATE TRIGGER IF NOT EXISTS trg_jobs_title_immutable
BEFORE UPDATE OF title ON jobs
FOR EACH ROW
WHEN NEW.title != OLD.title
BEGIN
    SELECT RAISE(ABORT, 'job title is immutable');
END;

CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS signal_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE,
    job_id INTEGER NOT NULL,
    env TEXT NOT NULL DEFAULT '',
    src TEXT NOT NULL DEFAULT 'store',
    hop INTEGER NOT NULL DEFAULT 0,
    event TEXT NOT NULL,
    created_at TEXT NOT NULL,
    dispatched_at TEXT NULL
);

CREATE INDEX IF NOT EXISTS idx_signal_outbox_pending
    ON signal_outbox(dispatched_at, id)
    WHERE dispatched_at IS NULL;

CREATE TABLE IF NOT EXISTS leases (
    job_id INTEGER PRIMARY KEY REFERENCES jobs(id),
    claim_token TEXT NOT NULL,
    holder TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS held_jobs (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    pool TEXT NOT NULL,
    alt_pools TEXT NOT NULL DEFAULT '[]',
    payload_json TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS burndown_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    pool TEXT NOT NULL,
    window_key TEXT NOT NULL,
    fired_at TEXT NOT NULL,
    job_id INTEGER NULL,
    note TEXT NOT NULL DEFAULT '',
    UNIQUE(pool, window_key)
);

CREATE TABLE IF NOT EXISTS reconciler_dedup (
    dedup_key TEXT PRIMARY KEY,
    first_seen_at TEXT NOT NULL,
    last_reported_at TEXT NOT NULL,
    count INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS gate_decisions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id INTEGER NOT NULL REFERENCES jobs(id),
    mode TEXT NOT NULL DEFAULT 'dry-run',
    requested_pool TEXT NOT NULL DEFAULT '',
    alt_pools TEXT NOT NULL DEFAULT '[]',
    decision TEXT NOT NULL CHECK(decision IN ('would_pass','would_hold','unknown')),
    blocked_by TEXT NOT NULL DEFAULT '',
    selected_pool TEXT NOT NULL DEFAULT '',
    bypassed INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    measured_at TEXT NULL,
    decided_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_gate_decisions_id_desc ON gate_decisions(id DESC);
CREATE INDEX IF NOT EXISTS idx_gate_decisions_job_id ON gate_decisions(job_id);
`
