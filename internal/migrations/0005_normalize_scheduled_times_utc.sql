-- +goose Up
-- PostgreSQL TIMESTAMPTZ values already represent absolute instants, so no
-- data conversion is required. This migration keeps dialect versions aligned.
SELECT 1;

-- +goose Down
SELECT 1;
