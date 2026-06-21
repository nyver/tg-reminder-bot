-- +goose Up
-- +goose StatementBegin

CREATE TABLE users (
    id         BIGINT PRIMARY KEY,
    tz         TEXT NOT NULL DEFAULT 'Europe/Moscow',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE reminders (
    id           UUID PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id),
    kind         TEXT NOT NULL,
    raw_text     TEXT NOT NULL,
    spec         JSONB NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    eval_cron    TEXT,
    next_eval_at TIMESTAMPTZ,
    locked_at    TIMESTAMPTZ,
    locked_by    TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_reminders_due ON reminders (next_eval_at) WHERE status = 'active';

CREATE TABLE scheduled_notifications (
    id              UUID PRIMARY KEY,
    reminder_id     UUID NOT NULL REFERENCES reminders(id) ON DELETE CASCADE,
    fire_at         TIMESTAMPTZ NOT NULL,
    text            TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    attempts        INT NOT NULL DEFAULT 0,
    locked_at       TIMESTAMPTZ,
    locked_by       TEXT,
    sent_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (idempotency_key)
);

CREATE INDEX idx_notifications_due ON scheduled_notifications (fire_at) WHERE status = 'pending';

CREATE TABLE observations (
    id          UUID PRIMARY KEY,
    reminder_id UUID NOT NULL REFERENCES reminders(id) ON DELETE CASCADE,
    value       BIGINT NOT NULL,
    currency    TEXT NOT NULL,
    available   BOOLEAN NOT NULL DEFAULT true,
    raw         JSONB,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_observations_last ON observations (reminder_id, observed_at DESC);

CREATE TABLE provider_cache (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl_seconds INT NOT NULL
);

-- FSM-состояния диалогов (для NLU-подтверждений, переживают рестарты)
CREATE TABLE dialog_states (
    user_id    BIGINT PRIMARY KEY REFERENCES users(id),
    state      TEXT NOT NULL DEFAULT 'idle',
    context    JSONB NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
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
