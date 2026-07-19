-- +goose Up
-- +goose StatementBegin
CREATE TABLE user_ui_preferences (
    user_id                 INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    quiet_start             TEXT,
    quiet_end               TEXT,
    morning_time            TEXT NOT NULL DEFAULT '09:00',
    default_snooze_minutes  INTEGER NOT NULL DEFAULT 10 CHECK (default_snooze_minutes BETWEEN 1 AND 10080),
    updated_at              DATETIME NOT NULL DEFAULT (datetime('now'))
);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS user_ui_preferences;
