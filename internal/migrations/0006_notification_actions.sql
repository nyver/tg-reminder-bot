-- +goose Up
-- +goose StatementBegin
ALTER TABLE reminders ADD COLUMN version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE scheduled_notifications ADD COLUMN parent_notification_id UUID REFERENCES scheduled_notifications(id) ON DELETE SET NULL;

CREATE TABLE notification_actions (
    id              UUID PRIMARY KEY,
    notification_id UUID NOT NULL REFERENCES scheduled_notifications(id) ON DELETE CASCADE,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action          TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (notification_id, user_id, action)
);

CREATE INDEX idx_notification_actions_user ON notification_actions (user_id, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS notification_actions;
ALTER TABLE scheduled_notifications DROP COLUMN IF EXISTS parent_notification_id;
ALTER TABLE reminders DROP COLUMN IF EXISTS version;
-- +goose StatementEnd
