-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS users (
    id         INTEGER PRIMARY KEY,
    tz         TEXT NOT NULL DEFAULT 'Europe/Moscow',
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS reminders (
    id           TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id),
    kind         TEXT NOT NULL,
    raw_text     TEXT NOT NULL,
    spec         TEXT NOT NULL,   -- JSON stored as TEXT
    status       TEXT NOT NULL DEFAULT 'active',
    eval_cron    TEXT,
    next_eval_at DATETIME,
    locked_at    DATETIME,
    locked_by    TEXT,
    created_at   DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at   DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders (next_eval_at)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS scheduled_notifications (
    id              TEXT PRIMARY KEY,
    reminder_id     TEXT NOT NULL REFERENCES reminders(id) ON DELETE CASCADE,
    fire_at         DATETIME NOT NULL,
    text            TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    locked_at       DATETIME,
    locked_by       TEXT,
    sent_at         DATETIME,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_notifications_due ON scheduled_notifications (fire_at)
    WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS observations (
    id          TEXT PRIMARY KEY,
    reminder_id TEXT NOT NULL REFERENCES reminders(id) ON DELETE CASCADE,
    value       INTEGER NOT NULL,
    currency    TEXT NOT NULL,
    available   INTEGER NOT NULL DEFAULT 1,   -- 0/1 for boolean
    raw         TEXT,                          -- JSON as TEXT
    observed_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_observations_last ON observations (reminder_id, observed_at DESC);

CREATE TABLE IF NOT EXISTS provider_cache (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,                 -- JSON as TEXT
    fetched_at  DATETIME NOT NULL DEFAULT (datetime('now')),
    ttl_seconds INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS dialog_states (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id),
    state      TEXT NOT NULL DEFAULT 'idle',
    context    TEXT NOT NULL DEFAULT '{}',     -- JSON as TEXT
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dialog_states;
DROP TABLE IF EXISTS provider_cache;
DROP TABLE IF EXISTS observations;
DROP TABLE IF EXISTS scheduled_notifications;
DROP TABLE IF EXISTS reminders;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
