-- Transaction manager plumbing: the transactions Waybill sends and every
-- hash each one has had (invariants I10, I11). See docs/design/txmgr.md §7.

-- +goose Up

-- One row per logical transaction. A row with a nonce must eventually be
-- mined (by one of its attempts), or the key's later transactions stall.
CREATE TABLE outbound_txs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    signer      text NOT NULL CHECK (signer ~ '^0x[0-9a-f]{40}$'),
    network     text NOT NULL,
    -- NULL only for a transaction that failed before a nonce was assigned
    -- (gas estimation reverted); such a row was never broadcast.
    nonce       bigint CHECK (nonce >= 0),
    -- Why the transaction exists, and what it is about. Submitting the same
    -- (purpose, ref_id) twice returns the first transaction.
    purpose     text NOT NULL CHECK (purpose ~ '^[a-z][a-z0-9_]*$'),
    ref_id      text NOT NULL CHECK (length(ref_id) BETWEEN 1 AND 200),
    to_address  text NOT NULL CHECK (to_address ~ '^0x[0-9a-f]{40}$'),
    data        bytea NOT NULL,
    value       minor_units NOT NULL DEFAULT 0 CHECK (value >= 0),
    gas_limit   bigint CHECK (gas_limit > 0),
    state       text NOT NULL CHECK (state IN ('pending', 'mined', 'reverted', 'failed')),
    failure     text,
    mined_hash  text CHECK (mined_hash ~ '^0x[0-9a-f]{64}$'),
    mined_block bigint,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (signer, network, nonce),
    UNIQUE (purpose, ref_id),
    CHECK ((state = 'failed') = (nonce IS NULL)),
    CHECK ((state IN ('mined', 'reverted')) = (mined_hash IS NOT NULL))
);
CREATE INDEX outbound_txs_pending_idx ON outbound_txs (signer, network, nonce) WHERE state = 'pending';

-- Every hash ever broadcast for a transaction: the first and each
-- replacement. Written before the broadcast, so a crash right after sending
-- still leaves the hash on record. Never updated or deleted.
CREATE TABLE outbound_tx_attempts (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    outbound_tx_id uuid NOT NULL REFERENCES outbound_txs (id),
    hash           text NOT NULL UNIQUE CHECK (hash ~ '^0x[0-9a-f]{64}$'),
    max_fee        minor_units NOT NULL CHECK (max_fee >= 0),
    tip            minor_units NOT NULL CHECK (tip >= 0 AND tip <= max_fee),
    broadcast_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbound_tx_attempts_tx_idx ON outbound_tx_attempts (outbound_tx_id, id);

-- +goose StatementBegin
CREATE FUNCTION outbound_tx_attempts_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'outbound_tx_attempts is append-only'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_tx_attempts_append_only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER outbound_tx_attempts_append_only BEFORE UPDATE OR DELETE ON outbound_tx_attempts
    FOR EACH ROW EXECUTE FUNCTION outbound_tx_attempts_append_only();

-- +goose Down
DROP TRIGGER outbound_tx_attempts_append_only ON outbound_tx_attempts;
DROP FUNCTION outbound_tx_attempts_append_only();
DROP TABLE outbound_tx_attempts;
DROP TABLE outbound_txs;
