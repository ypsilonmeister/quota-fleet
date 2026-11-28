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
    session_id TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    selected_agent TEXT NOT NULL DEFAULT '',
    selected_model TEXT NOT NULL DEFAULT '',
    selected_pool TEXT NOT NULL DEFAULT '',
    selection_reason TEXT NOT NULL DEFAULT '',
    selection_mode TEXT NOT NULL DEFAULT '',
    effort TEXT NOT NULL DEFAULT ''
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

CREATE TABLE IF NOT EXISTS pool_cooldowns (
    pool TEXT PRIMARY KEY,
    active_until TEXT NOT NULL,
    triggered_at TEXT NOT NULL,
    consecutive_hits INTEGER NOT NULL DEFAULT 1,
    last_job_id INTEGER NULL REFERENCES jobs(id) ON DELETE SET NULL,
    last_agent TEXT NOT NULL DEFAULT '',
    last_pattern TEXT NOT NULL DEFAULT '',
    cleared_at TEXT NULL,
    cleared_reason TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

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
    status TEXT NOT NULL DEFAULT 'held' CHECK(status IN ('held','released','cancelled')),
    pool TEXT NOT NULL,
    alt_pools TEXT NOT NULL DEFAULT '[]',
    payload_json TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    snapshot_summary TEXT NOT NULL DEFAULT '',
    released_job_id INTEGER NULL REFERENCES jobs(id),
    released_pool TEXT NOT NULL DEFAULT '',
    released_at TEXT NULL,
    cancelled_at TEXT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    error_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    cancel_reason TEXT NOT NULL DEFAULT ''
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
    job_id INTEGER NULL REFERENCES jobs(id),
    held_id TEXT NOT NULL DEFAULT '',
    held INTEGER NOT NULL DEFAULT 0,
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

CREATE TABLE IF NOT EXISTS decision_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    job_id INTEGER NULL,
    review_job_id INTEGER NULL,
    verdict TEXT NOT NULL CHECK(verdict IN ('ok_to_merge','changes_needed')),
    score REAL NOT NULL,
    model TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_versions (
    host TEXT NOT NULL,
    agent TEXT NOT NULL,
    version_raw TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('ok','not_found','error')),
    error_detail TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL,
    PRIMARY KEY (host, agent)
);

CREATE TABLE IF NOT EXISTS agent_version_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL CHECK(event_type IN ('version_changed','drift_started','drift_resolved','probe_failed')),
    agent TEXT NOT NULL,
    host TEXT NOT NULL DEFAULT '',
    old_version TEXT NOT NULL DEFAULT '',
    new_version TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    manual_update INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_version_events_created_at ON agent_version_events(created_at, id);
CREATE INDEX IF NOT EXISTS idx_agent_version_events_agent ON agent_version_events(agent, id DESC);

CREATE TABLE IF NOT EXISTS agent_models (
    host TEXT NOT NULL,
    agent TEXT NOT NULL,
    models_json TEXT NOT NULL DEFAULT '[]',
    source TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ok' CHECK(status IN ('ok','error')),
    error_detail TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (host, agent)
);

CREATE TABLE IF NOT EXISTS push_subscriptions (
    endpoint TEXT PRIMARY KEY,
    p256dh TEXT NOT NULL,
    auth TEXT NOT NULL,
    created_at TEXT NOT NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    token_id TEXT NOT NULL DEFAULT ''
);

-- Endpoints removed through the API, which are never registered again (docs/16).
CREATE TABLE IF NOT EXISTS push_deleted_endpoints (
    endpoint_hash TEXT PRIMARY KEY,
    deleted_at TEXT NOT NULL
);
`
