-- Bitcoin receive (docs/design/stage5.md §2, ADR-045). New tables beside
-- the EVM ones, so the EVM tables keep their strict 0x checks. Not ledger
-- tables: Bitcoin custody is booked through ledger.Post like any other.

-- +goose Up

-- The next unused derivation index per network. Addresses are never reused.
CREATE TABLE btc_address_counters (
    network    text PRIMARY KEY CHECK (network IN ('btc:regtest', 'btc:signet')),
    next_index bigint NOT NULL DEFAULT 0 CHECK (next_index BETWEEN 0 AND 2147483647)
);

-- One receive address per BTC invoice and network: xpub/0/derivation_index.
CREATE TABLE btc_addresses (
    address          text PRIMARY KEY CHECK (address ~ '^(bcrt1|tb1)[02-9ac-hj-np-z]{8,87}$'),
    network          text NOT NULL CHECK (network IN ('btc:regtest', 'btc:signet')),
    invoice_id       uuid NOT NULL REFERENCES invoices (id),
    derivation_index bigint NOT NULL CHECK (derivation_index >= 0),
    UNIQUE (invoice_id, network),
    UNIQUE (network, derivation_index)
);

-- Outputs paying our addresses, as the watcher saw them.
CREATE TABLE btc_payments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    network      text NOT NULL,
    txid         text NOT NULL CHECK (txid ~ '^[0-9a-f]{64}$'),
    vout         integer NOT NULL CHECK (vout >= 0),
    block_height bigint NOT NULL CHECK (block_height >= 0),
    block_hash   text NOT NULL CHECK (block_hash ~ '^[0-9a-f]{64}$'),
    address      text NOT NULL REFERENCES btc_addresses (address),
    invoice_id   uuid NOT NULL REFERENCES invoices (id),
    amount       minor_units NOT NULL CHECK (amount > 0), -- satoshis
    state        text NOT NULL DEFAULT 'detected' CHECK (state IN ('detected', 'confirmed', 'final', 'reorged')),
    detected_at  timestamptz NOT NULL DEFAULT now(),
    confirmed_at timestamptz,
    final_at     timestamptz
);
-- An output seen twice is the same payment (I9); after a reorg it may be
-- seen again in a new block, so reorged rows do not count.
CREATE UNIQUE INDEX btc_payments_once ON btc_payments (network, txid, vout) WHERE state <> 'reorged';
CREATE INDEX btc_payments_open_idx ON btc_payments (network, state) WHERE state IN ('detected', 'confirmed');
CREATE INDEX btc_payments_invoice_idx ON btc_payments (invoice_id);

-- The next block to read, and the hash of every block read recently, to
-- detect reorgs.
CREATE TABLE btc_cursors (
    network     text PRIMARY KEY,
    next_height bigint NOT NULL CHECK (next_height >= 0)
);
CREATE TABLE btc_blocks (
    network text NOT NULL,
    height  bigint NOT NULL CHECK (height >= 0),
    hash    text NOT NULL CHECK (hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (network, height)
);

-- +goose Down
DROP TABLE btc_blocks;
DROP TABLE btc_cursors;
DROP TABLE btc_payments;
DROP TABLE btc_addresses;
DROP TABLE btc_address_counters;
