-- +goose Up
-- +goose StatementBegin

CREATE TABLE epg_channels (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL
);

CREATE TABLE epg_programmes (
    id          BIGSERIAL PRIMARY KEY,
    channel_id  TEXT        NOT NULL,
    title       TEXT        NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ,
    description TEXT
);

CREATE INDEX idx_epg_programmes_lookup ON epg_programmes (channel_id, starts_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS epg_programmes;
DROP TABLE IF EXISTS epg_channels;
-- +goose StatementEnd
