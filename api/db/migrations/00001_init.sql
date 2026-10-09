-- Initial migration. Intentionally empty: it proves the migration path runs
-- end to end (binary, Compose, testcontainers, CI) before any schema exists.
-- The ledger schema arrives in stage 1 (see docs/ROADMAP.md).

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
