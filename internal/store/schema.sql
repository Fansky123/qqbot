PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    creator_id TEXT NOT NULL,
    requirement TEXT NOT NULL,
    plan TEXT NOT NULL,
    status TEXT NOT NULL,
    branch TEXT NOT NULL,
    worktree TEXT NOT NULL,
    base_commit TEXT NOT NULL,
    git_common_dir TEXT NOT NULL,
    task_commit TEXT NOT NULL,
    rc_commit TEXT NOT NULL,
    deploy_key TEXT NOT NULL,
    session_id TEXT NOT NULL,
    summary TEXT NOT NULL,
    failure TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS tasks_status_created_at_idx
ON tasks (status, created_at, id);

CREATE INDEX IF NOT EXISTS tasks_status_updated_at_idx
ON tasks (status, updated_at, id);

CREATE TABLE IF NOT EXISTS task_inputs (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    kind TEXT NOT NULL,
    user_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS approvals (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    kind TEXT NOT NULL,
    user_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    bound_commit TEXT NOT NULL,
    result TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (group_id, message_id)
);

CREATE INDEX IF NOT EXISTS approvals_task_kind_result_idx
ON approvals (task_id, kind, result);

CREATE TABLE IF NOT EXISTS processed_messages (
    message_key TEXT PRIMARY KEY,
    processed_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    kind TEXT NOT NULL,
    detail TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS scheduler_leases (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    owner TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS approval_execution_locks (
    project_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    group_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    acquired_at INTEGER NOT NULL
);
