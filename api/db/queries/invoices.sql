-- Contractors and API keys -----------------------------------------------

-- name: UpsertContractor :one
INSERT INTO contractors (id, legal_name, verified_at)
VALUES (@id, @legal_name, sqlc.narg(verified_at))
ON CONFLICT (id) DO UPDATE SET legal_name = EXCLUDED.legal_name, verified_at = EXCLUDED.verified_at
RETURNING *;

-- name: InsertAPIKey :exec
INSERT INTO api_keys (contractor_id, prefix, secret_sha256)
VALUES (@contractor_id, @prefix, @secret_sha256)
ON CONFLICT (prefix) DO NOTHING;

-- name: GetActiveAPIKey :one
SELECT k.contractor_id, k.secret_sha256
FROM api_keys k
WHERE k.prefix = @prefix AND k.revoked_at IS NULL;

-- name: GetContractor :one
SELECT * FROM contractors WHERE id = @id;

-- Chain tokens -----------------------------------------------------------

-- name: UpsertChainToken :exec
INSERT INTO chain_tokens (network, contract_address, asset_code)
VALUES (@network, @contract_address, @asset_code)
ON CONFLICT (network, asset_code) DO UPDATE SET contract_address = EXCLUDED.contract_address;

-- name: ListChainTokens :many
SELECT * FROM chain_tokens WHERE network = @network;

-- Invoices ----------------------------------------------------------------

-- name: InsertInvoice :one
INSERT INTO invoices (id, contractor_id, tracking_code, description, asset_code, amount, payout_mode, expires_at)
VALUES (@id, @contractor_id, @tracking_code, @description, @asset_code, @amount, @payout_mode, @expires_at)
RETURNING *;

-- name: InsertDepositAddress :exec
INSERT INTO deposit_addresses (network, address, invoice_id, salt)
VALUES (@network, @address, @invoice_id, @salt);

-- name: ListDepositAddresses :many
SELECT * FROM deposit_addresses WHERE invoice_id = @invoice_id ORDER BY network;

-- name: GetInvoiceByTrackingCode :one
SELECT i.*, c.legal_name AS contractor_name, c.verified_at AS contractor_verified_at
FROM invoices i JOIN contractors c ON c.id = i.contractor_id
WHERE i.tracking_code = @tracking_code;

-- name: GetInvoiceForUpdate :one
SELECT * FROM invoices WHERE id = @id FOR UPDATE;

-- name: UpdateInvoiceState :exec
UPDATE invoices SET state = @state, updated_at = now() WHERE id = @id;

-- name: InsertInvoiceEvent :one
INSERT INTO invoice_events (invoice_id, type, from_state, to_state, data)
VALUES (@invoice_id, @type, sqlc.narg(from_state), @to_state, @data)
RETURNING id, created_at;

-- name: ListInvoiceEvents :many
SELECT * FROM invoice_events WHERE invoice_id = @invoice_id ORDER BY id;

-- name: LatestInvoiceEventID :one
SELECT coalesce(max(id), 0)::bigint FROM invoice_events WHERE invoice_id = @invoice_id;

-- Payments (watcher) --------------------------------------------------------

-- name: ListWatchedAddresses :many
-- Every deposit address on a network, including expired invoices: late
-- payments are still recorded (docs/RISKS.md §7).
SELECT address, invoice_id FROM deposit_addresses WHERE network = @network;

-- name: InsertPayment :one
-- ON CONFLICT: the same log seen twice is the same payment (I9).
INSERT INTO payments (network, tx_hash, log_index, block_number, block_hash, token_address,
                      from_address, to_address, invoice_id, asset_code, amount)
VALUES (@network, @tx_hash, @log_index, @block_number, @block_hash, @token_address,
        @from_address, @to_address, @invoice_id, @asset_code, @amount)
ON CONFLICT (network, tx_hash, log_index) DO NOTHING
RETURNING *;

-- name: ListOpenPayments :many
SELECT * FROM payments WHERE network = @network AND state IN ('detected', 'confirmed') ORDER BY block_number, log_index;

-- name: MarkPaymentConfirmed :exec
UPDATE payments SET state = 'confirmed', confirmed_at = now() WHERE id = @id AND state = 'detected';

-- name: MarkPaymentFinal :exec
UPDATE payments SET state = 'final', final_at = now() WHERE id = @id AND state = 'confirmed';

-- name: SumPaymentsByState :one
-- Total of an invoice's payments that have reached at least the given state.
SELECT coalesce(sum(amount), 0)::minor_units AS total
FROM payments
WHERE invoice_id = @invoice_id
  AND state = ANY(@states::text[]);

-- name: GetChainCursor :one
SELECT next_block FROM chain_cursors WHERE network = @network;

-- name: SetChainCursor :exec
INSERT INTO chain_cursors (network, next_block) VALUES (@network, @next_block)
ON CONFLICT (network) DO UPDATE SET next_block = EXCLUDED.next_block, updated_at = now();

-- Idempotency ----------------------------------------------------------------

-- name: ClaimIdempotencyKey :one
-- Inserts the key inside the caller's transaction. A concurrent request
-- with the same key blocks here until the first transaction ends.
INSERT INTO idempotency_keys (principal, key, request_hash)
VALUES (@principal, @key, @request_hash)
ON CONFLICT (principal, key) DO NOTHING
RETURNING principal;

-- name: GetIdempotencyRecord :one
SELECT * FROM idempotency_keys WHERE principal = @principal AND key = @key;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
SET status_code = @status_code, response_body = @response_body, completed_at = now()
WHERE principal = @principal AND key = @key;
