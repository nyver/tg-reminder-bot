-- +goose Up
-- +goose StatementBegin
ALTER TABLE reminders ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE scheduled_notifications ADD COLUMN parent_notification_id TEXT REFERENCES scheduled_notifications(id) ON DELETE SET NULL;

CREATE TABLE notification_actions (
    id              TEXT PRIMARY KEY,
    notification_id TEXT NOT NULL REFERENCES scheduled_notifications(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action          TEXT NOT NULL,
    payload         TEXT NOT NULL DEFAULT '{}',
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    UNIQUE (notification_id, user_id, action)
);

CREATE INDEX idx_notification_actions_user ON notification_actions (user_id, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS notification_actions;
-- Added columns are retained for compatibility with SQLite versions without DROP COLUMN.
-- +goose StatementEnd
