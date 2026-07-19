-- +goose Up
-- +goose StatementBegin
-- Older application versions persisted time.Time.String() values with their
-- original numeric offset. SQLite compares DATETIME columns lexicographically
-- against datetime('now'), so convert scheduled instants to offset-free UTC.
UPDATE reminders
SET next_eval_at = datetime(
    substr(next_eval_at, 1, 19) ||
    CASE
        WHEN instr(next_eval_at, ' +') > 0 THEN
            substr(next_eval_at, instr(next_eval_at, ' +') + 1, 3) || ':' ||
            substr(next_eval_at, instr(next_eval_at, ' +') + 4, 2)
        ELSE
            substr(next_eval_at, instr(next_eval_at, ' -') + 1, 3) || ':' ||
            substr(next_eval_at, instr(next_eval_at, ' -') + 4, 2)
    END
)
WHERE instr(next_eval_at, ' +') > 0 OR instr(next_eval_at, ' -') > 0;

UPDATE scheduled_notifications
SET fire_at = datetime(
    substr(fire_at, 1, 19) ||
    CASE
        WHEN instr(fire_at, ' +') > 0 THEN
            substr(fire_at, instr(fire_at, ' +') + 1, 3) || ':' ||
            substr(fire_at, instr(fire_at, ' +') + 4, 2)
        ELSE
            substr(fire_at, instr(fire_at, ' -') + 1, 3) || ':' ||
            substr(fire_at, instr(fire_at, ' -') + 4, 2)
    END
)
WHERE instr(fire_at, ' +') > 0 OR instr(fire_at, ' -') > 0;
-- +goose StatementEnd

-- +goose Down
-- UTC normalization is intentionally irreversible.
SELECT 1;
