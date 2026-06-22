-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS epg_channels (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS epg_programmes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id  TEXT     NOT NULL,
    title       TEXT     NOT NULL,
    starts_at   DATETIME NOT NULL,
    ends_at     DATETIME,
    description TEXT
);

CREATE INDEX IF NOT EXISTS idx_epg_programmes_lookup ON epg_programmes (channel_id, starts_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS epg_programmes;
DROP TABLE IF EXISTS epg_channels;
-- +goose StatementEnd
