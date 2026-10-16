-- The transactional outbox (ADR-006, docs/ARCHITECTURE.md §7). Work is
-- enqueued in the same transaction as the state change that causes it, and
-- workers lease it with FOR UPDATE SKIP LOCKED.

-- +goose Up

CREATE TABLE outbox (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    topic            text NOT NULL CHECK (topic ~ '^[a-z][a-z0-9_.]{0,99}$'),
    payload          jsonb NOT NULL,
    -- Optional: enqueueing the same logical job twice is a no-op.
    dedupe_key       text UNIQUE CHECK (length(dedupe_key) BETWEEN 1 AND 255),
    available_at     timestamptz NOT NULL DEFAULT now(),
    attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_owner      text,
    lease_expires_at timestamptz,
    done_at          timestamptz,
    -- Gave up after the last attempt, or nothing handles the topic. Kept for
    -- a person to look at; never retried automatically.
    dead_at          timestamptz,
    last_error       text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (done_at IS NULL OR dead_at IS NULL),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);
CREATE INDEX outbox_ready_idx ON outbox (available_at, id) WHERE done_at IS NULL AND dead_at IS NULL;

-- +goose Down
DROP TABLE outbox;
