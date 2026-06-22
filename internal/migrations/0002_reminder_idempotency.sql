-- +goose Up
-- +goose StatementBegin
ALTER TABLE reminders ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX idx_reminders_idempotency ON reminders (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_reminders_idempotency;
ALTER TABLE reminders DROP COLUMN IF EXISTS idempotency_key;
-- +goose StatementEnd
