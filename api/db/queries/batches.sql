-- Contractor payout wallets and batch runs (docs/design/teams.md).

-- name: GetContractorWallet :one
SELECT * FROM contractor_wallets WHERE contractor_id = @contractor_id AND network = @network;

-- name: UpsertContractorWallet :one
-- Saving the same address again changes nothing; a new address starts a
-- new 24-hour wait before it can be paid.
INSERT INTO contractor_wallets (contractor_id, network, address, added_at, payouts_allowed_from)
VALUES (@contractor_id, @network, @address, @now::timestamptz, @payouts_allowed_from)
ON CONFLICT (contractor_id, network) DO UPDATE
SET address = EXCLUDED.address, added_at = EXCLUDED.added_at, payouts_allowed_from = EXCLUDED.payouts_allowed_from
WHERE contractor_wallets.address <> EXCLUDED.address
RETURNING *;

-- name: GetContractorByID :one
SELECT * FROM contractors WHERE id = @id;

-- name: InsertBatch :one
INSERT INTO batches (id, org_id, created_by, network, asset_code, total, state, chain_batch_id, failure)
VALUES (@id, @org_id, @created_by, @network, @asset_code, @total, 'draft', @chain_batch_id, sqlc.narg(failure))
RETURNING *;

-- name: InsertBatchLine :exec
INSERT INTO batch_lines (batch_id, line_no, contractor_id, wallet_address, amount, reference, problem, payout_id, tracking_code)
VALUES (@batch_id, @line_no, @contractor_id, sqlc.narg(wallet_address), @amount, @reference, sqlc.narg(problem), @payout_id, @tracking_code);

-- name: GetBatch :one
SELECT * FROM batches WHERE id = @id AND org_id = @org_id;

-- name: GetBatchForUpdate :one
SELECT * FROM batches WHERE id = @id FOR UPDATE;

-- name: ListBatches :many
SELECT * FROM batches WHERE org_id = @org_id ORDER BY created_at DESC, id LIMIT 50;

-- name: ListBatchLines :many
SELECT l.*, c.legal_name AS contractor_name, c.verified_at AS contractor_verified_at
FROM batch_lines l JOIN contractors c ON c.id = l.contractor_id
WHERE l.batch_id = @batch_id ORDER BY l.line_no;

-- name: SetBatchState :exec
UPDATE batches SET state = @state, failure = sqlc.narg(failure), updated_at = now() WHERE id = @id;

-- name: SetBatchOutboundTx :exec
UPDATE batches SET outbound_tx_id = @outbound_tx_id, updated_at = now() WHERE id = @id;

-- name: InsertBatchEvent :exec
INSERT INTO batch_events (batch_id, type, from_state, to_state, member_id, data)
VALUES (@batch_id, @type, @from_state, @to_state, sqlc.narg(member_id), @data);

-- name: ListBatchEvents :many
SELECT * FROM batch_events WHERE batch_id = @batch_id ORDER BY id;

-- name: InsertApproval :exec
INSERT INTO approvals (batch_id, creator_id, approver_id, decision)
VALUES (@batch_id, @creator_id, @approver_id, @decision);

-- name: ListApprovals :many
SELECT * FROM approvals WHERE batch_id = @batch_id ORDER BY decided_at;

-- name: GetBatchByOutboundTx :one
SELECT * FROM batches WHERE outbound_tx_id = @outbound_tx_id FOR UPDATE;

-- name: ListBatchesInState :many
SELECT * FROM batches WHERE state = @state ORDER BY updated_at LIMIT 100;

-- name: CountRecentLinePayments :one
-- Lines to this contractor with this reference that this organisation has
-- paid or may still pay in the last 30 days.
SELECT count(*) FROM batch_lines l JOIN batches b ON b.id = l.batch_id
WHERE b.org_id = @org_id AND l.contractor_id = @contractor_id AND l.reference = @reference
  AND b.state NOT IN ('draft', 'rejected', 'cancelled', 'failed')
  AND b.created_at > now() - interval '30 days';

-- name: GetBatchLineByTrackingCode :one
SELECT l.*, c.legal_name AS contractor_name, b.state AS batch_state, b.network, b.updated_at AS batch_updated_at
FROM batch_lines l
JOIN batches b ON b.id = l.batch_id
JOIN contractors c ON c.id = l.contractor_id
WHERE l.tracking_code = @tracking_code;

-- name: GetOutboundTx :one
SELECT * FROM outbound_txs WHERE id = @id;

-- name: SumBatchesInFlight :one
-- What runs have reserved and not yet recorded as paid, as a positive
-- amount.
SELECT coalesce(-sum(b.balance), 0)::minor_units AS total
FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
WHERE a.code LIKE 'liability:batch:%:in-flight';
