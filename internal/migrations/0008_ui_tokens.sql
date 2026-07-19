-- +goose Up
-- +goose StatementBegin
CREATE TABLE ui_tokens (
    token       TEXT PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL,
    entity_id   UUID NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ui_tokens_expiry ON ui_tokens (expires_at);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS ui_tokens;
