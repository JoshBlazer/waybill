-- Contractors, API keys, invoices, deposit addresses, on-chain payments,
-- the watcher's cursor and idempotency records. Stage 1 thin slice.
-- See docs/ARCHITECTURE.md §4.

-- +goose Up

CREATE TABLE contractors (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    legal_name  text NOT NULL CHECK (length(legal_name) BETWEEN 1 AND 200),
    -- Set when the name has been verified against the bank account
    -- (fake verifier until stage 3).
    verified_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Contractor API keys. Only a SHA-256 hash of the secret is stored; the full
-- key is shown once, when it is created.
CREATE TABLE api_keys (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contractor_id uuid NOT NULL REFERENCES contractors (id),
    -- Public, non-secret identifier embedded in the key: wb_test_<prefix>_<secret>.
    prefix        text NOT NULL UNIQUE CHECK (prefix ~ '^[0-9a-z]{8}$'),
    secret_sha256 bytea NOT NULL CHECK (length(secret_sha256) = 32),
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz
);

-- Which token contract on which network is which ledger asset.
CREATE TABLE chain_tokens (
    network          text NOT NULL,
    contract_address text NOT NULL CHECK (contract_address ~ '^0x[0-9a-f]{40}$'),
    asset_code       text NOT NULL REFERENCES assets (code),
    PRIMARY KEY (network, contract_address),
    UNIQUE (network, asset_code)
);

CREATE TABLE invoices (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contractor_id uuid NOT NULL REFERENCES contractors (id),
    tracking_code text NOT NULL UNIQUE CHECK (tracking_code ~ '^WB(-[0-9A-HJKMNP-TV-Z]{4}){4}$'),
    description   text NOT NULL CHECK (length(description) BETWEEN 1 AND 500),
    asset_code    text NOT NULL REFERENCES assets (code),
    amount        minor_units NOT NULL CHECK (amount > 0),
    payout_mode   text NOT NULL CHECK (payout_mode IN ('hold', 'naira')),
    state         text NOT NULL DEFAULT 'open' CHECK (state IN
                    ('open', 'received', 'underpaid', 'overpaid', 'confirmed', 'settled', 'expired', 'cancelled')),
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Every state change, for the tracking page and SSE replay (Last-Event-ID).
CREATE TABLE invoice_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invoice_id uuid NOT NULL REFERENCES invoices (id),
    type       text NOT NULL,
    from_state text,
    to_state   text NOT NULL,
    data       jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX invoice_events_invoice_idx ON invoice_events (invoice_id, id);

-- Live updates: every new invoice event notifies listeners (the API's SSE
-- broker) with the invoice id. NOTIFY is delivered at commit, so listeners
-- never hear about changes that roll back.
-- +goose StatementBegin
CREATE FUNCTION invoice_events_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('invoice_events', NEW.invoice_id::text);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER invoice_events_notify
    AFTER INSERT ON invoice_events
    FOR EACH ROW EXECUTE FUNCTION invoice_events_notify();

-- One counterfactual CREATE2 address per invoice and network.
CREATE TABLE deposit_addresses (
    network    text NOT NULL,
    address    text NOT NULL CHECK (address ~ '^0x[0-9a-f]{40}$'),
    invoice_id uuid NOT NULL REFERENCES invoices (id),
    salt       bytea NOT NULL CHECK (length(salt) = 32),
    PRIMARY KEY (network, address),
    UNIQUE (invoice_id, network)
);

-- Token transfers into deposit addresses, as seen by the watcher.
CREATE TABLE payments (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    network       text NOT NULL,
    tx_hash       text NOT NULL CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    log_index     integer NOT NULL CHECK (log_index >= 0),
    block_number  bigint NOT NULL,
    block_hash    text NOT NULL CHECK (block_hash ~ '^0x[0-9a-f]{64}$'),
    token_address text NOT NULL,
    from_address  text NOT NULL,
    to_address    text NOT NULL,
    invoice_id    uuid NOT NULL REFERENCES invoices (id),
    asset_code    text NOT NULL REFERENCES assets (code),
    amount        minor_units NOT NULL CHECK (amount > 0),
    state         text NOT NULL DEFAULT 'detected' CHECK (state IN ('detected', 'confirmed', 'final', 'reorged')),
    detected_at   timestamptz NOT NULL DEFAULT now(),
    confirmed_at  timestamptz,
    final_at      timestamptz,
    -- A log seen twice is the same payment (invariant I9).
    UNIQUE (network, tx_hash, log_index),
    FOREIGN KEY (network, to_address) REFERENCES deposit_addresses (network, address)
);
CREATE INDEX payments_invoice_idx ON payments (invoice_id);
CREATE INDEX payments_open_idx ON payments (network, state) WHERE state IN ('detected', 'confirmed');

-- The next block each network's watcher will read.
CREATE TABLE chain_cursors (
    network    text PRIMARY KEY,
    next_block bigint NOT NULL CHECK (next_block >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Idempotency records for money-moving requests (invariant I6).
CREATE TABLE idempotency_keys (
    principal     text NOT NULL,
    key           text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_hash  bytea NOT NULL CHECK (length(request_hash) = 32),
    -- NULL until the request finishes; a second request meanwhile gets 409.
    status_code   integer,
    response_body bytea,
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz,
    PRIMARY KEY (principal, key)
);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE chain_cursors;
DROP TABLE payments;
DROP TABLE deposit_addresses;
DROP TABLE invoice_events;
DROP FUNCTION invoice_events_notify();
DROP TABLE invoices;
DROP TABLE chain_tokens;
DROP TABLE api_keys;
DROP TABLE contractors;
