-- Reorg handling in the watcher. See docs/ARCHITECTURE.md §8.

-- +goose Up

-- The watcher's memory of the chain: the hash of the last block of every
-- range it has scanned. If the node later reports a different hash at one of
-- these heights, the blocks above the common ancestor were replaced.
CREATE TABLE chain_blocks (
    network text NOT NULL,
    height  bigint NOT NULL CHECK (height >= 0),
    hash    text NOT NULL CHECK (hash ~ '^0x[0-9a-f]{64}$'),
    PRIMARY KEY (network, height)
);

ALTER TABLE payments ADD COLUMN reorged_at timestamptz;

-- A transaction dropped by a reorg is usually included again in a later
-- block with the same hash and log index. The reorged row stays as history;
-- the new sighting is a new payment. A log is still recorded at most once
-- among payments that are not reorged (invariant I9).
ALTER TABLE payments DROP CONSTRAINT payments_network_tx_hash_log_index_key;
CREATE UNIQUE INDEX payments_live_log_key ON payments (network, tx_hash, log_index) WHERE state <> 'reorged';

-- +goose Down
-- Fails if a reorged payment and its new sighting both exist; that history
-- cannot be expressed under the old constraint.
DROP INDEX payments_live_log_key;
ALTER TABLE payments ADD CONSTRAINT payments_network_tx_hash_log_index_key UNIQUE (network, tx_hash, log_index);
ALTER TABLE payments DROP COLUMN reorged_at;
DROP TABLE chain_blocks;
