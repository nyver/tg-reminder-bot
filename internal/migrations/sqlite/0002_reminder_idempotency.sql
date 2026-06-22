-- +goose Up
-- +goose StatementBegin
ALTER TABLE reminders ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_reminders_idempotency ON reminders (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_reminders_idempotency;
-- SQLite does not support DROP COLUMN before 3.35; the column stays but the index is gone.
-- +goose StatementEnd
