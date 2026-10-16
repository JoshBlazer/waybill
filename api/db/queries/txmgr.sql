-- Transaction manager. See docs/design/txmgr.md.

-- name: AcquireSignerLease :one
-- Takes the lease when nobody holds it, renews it for its holder, and takes
-- it over once it has expired. Returns no row when another process holds a
-- live lease.
INSERT INTO signer_nonces (signer, network, lease_owner, lease_expires_at)
VALUES (@signer, @network, @owner, statement_timestamp() + sqlc.arg(ttl_ms)::bigint * interval '1 millisecond')
ON CONFLICT (signer, network) DO UPDATE
SET lease_owner = EXCLUDED.lease_owner, lease_expires_at = EXCLUDED.lease_expires_at
WHERE signer_nonces.lease_owner = EXCLUDED.lease_owner
   OR signer_nonces.lease_expires_at <= statement_timestamp()
RETURNING lease_owner, lease_expires_at;

-- name: LockSignerNonce :one
-- Locks the key's row until the caller's transaction ends. db_now is the
-- database clock, so lease checks never depend on the caller's clock.
SELECT next_nonce, lease_owner, lease_expires_at, statement_timestamp()::timestamptz AS db_now
FROM signer_nonces
WHERE signer = @signer AND network = @network
FOR UPDATE;

-- name: SetSignerNextNonce :exec
UPDATE signer_nonces SET next_nonce = @next_nonce WHERE signer = @signer AND network = @network;

-- Outbound transactions --------------------------------------------------

-- name: GetOutboundTxByRef :one
SELECT * FROM outbound_txs WHERE purpose = @purpose AND ref_id = @ref_id;

-- name: InsertOutboundTx :one
INSERT INTO outbound_txs (signer, network, nonce, purpose, ref_id, to_address, data, value, gas_limit, state)
VALUES (@signer, @network, @nonce, @purpose, @ref_id, @to_address, @data, @value, @gas_limit, 'pending')
RETURNING *;

-- name: InsertFailedOutboundTx :one
-- A transaction that failed before a nonce was assigned. Nothing was sent.
INSERT INTO outbound_txs (signer, network, nonce, purpose, ref_id, to_address, data, value, state, failure)
VALUES (@signer, @network, NULL, @purpose, @ref_id, @to_address, @data, @value, 'failed', @failure)
RETURNING *;

-- name: ListPendingOutboundTxs :many
SELECT * FROM outbound_txs
WHERE signer = @signer AND network = @network AND state = 'pending'
ORDER BY nonce;

-- name: FinishOutboundTx :execrows
UPDATE outbound_txs
SET state = @state, mined_hash = @mined_hash, mined_block = @mined_block, updated_at = now()
WHERE id = @id AND state = 'pending';

-- name: InsertOutboundAttempt :one
INSERT INTO outbound_tx_attempts (outbound_tx_id, hash, max_fee, tip)
VALUES (@outbound_tx_id, @hash, @max_fee, @tip)
RETURNING *;

-- name: ListOutboundAttempts :many
SELECT * FROM outbound_tx_attempts WHERE outbound_tx_id = @outbound_tx_id ORDER BY id;
