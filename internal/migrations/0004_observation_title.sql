-- +goose Up
-- +goose StatementBegin
ALTER TABLE observations ADD COLUMN title TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE observations DROP COLUMN title;
-- +goose StatementEnd
