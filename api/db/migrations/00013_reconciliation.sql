-- Reconciliation reports (docs/design/reconciliation.md). Not a ledger
-- table: the job only reads the ledger.

-- +goose Up

CREATE TABLE reconciliation_runs (
    id          uuid PRIMARY KEY,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    -- The block height each network was read at.
    as_of       jsonb NOT NULL,
    differences integer NOT NULL CHECK (differences >= 0),
    -- Every check: its status, figures, money in flight and differences.
    report      jsonb NOT NULL
);
CREATE INDEX reconciliation_runs_recent_idx ON reconciliation_runs (started_at DESC);

-- +goose Down
DROP TABLE reconciliation_runs;
