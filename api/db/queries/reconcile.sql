-- Reconciliation reads (docs/design/reconciliation.md). Read-only, apart
-- from storing each report.

-- name: ListDepositAddressesByNetwork :many
SELECT address FROM deposit_addresses WHERE network = @network ORDER BY address;

-- name: ListDetectedPaymentsUpTo :many
-- Payments the chain holds at the height read but the ledger has not
-- booked yet: deposits are booked at confirmation.
SELECT id, to_address, amount, block_number FROM payments
WHERE network = @network AND state = 'detected' AND block_number <= @height::bigint
ORDER BY block_number, log_index;

-- name: ListBookedPaymentsAbove :many
-- Payments the ledger has booked (confirmed or final) from blocks above
-- the height read, which the balance read cannot include yet.
SELECT id, to_address, amount, block_number FROM payments
WHERE network = @network AND state IN ('confirmed', 'final') AND block_number > @height::bigint
ORDER BY block_number, log_index;

-- name: ListPendingTxAttempts :many
-- Every hash broadcast for a still-pending transaction of the given
-- purposes: one of them may already be mined.
SELECT t.id, t.purpose, t.ref_id, a.hash FROM outbound_txs t
JOIN outbound_tx_attempts a ON a.outbound_tx_id = t.id
WHERE t.network = @network AND t.state = 'pending' AND t.purpose = ANY(@purposes::text[])
ORDER BY t.id, a.id;

-- name: ListBatchesAwaitingFinality :many
-- Runs whose payout may already be on chain but is not yet booked out of
-- the vault, with their transaction's state and hash.
SELECT b.id, b.state, b.total, t.id AS tx_id, t.state AS tx_state, t.mined_hash, t.mined_block
FROM batches b JOIN outbound_txs t ON t.id = b.outbound_tx_id
WHERE b.network = @network AND b.state IN ('sending', 'confirming');

-- name: ListFinalPaymentsSince :many
SELECT id, tx_hash, log_index, block_hash, token_address, to_address, amount FROM payments
WHERE network = @network AND state = 'final' AND final_at >= @since::timestamptz
ORDER BY final_at, id;

-- name: ListPaidBatchesSince :many
SELECT b.id, b.total, b.chain_batch_id, t.mined_hash,
       (SELECT count(*) FROM batch_lines l WHERE l.batch_id = b.id)::bigint AS lines
FROM batches b JOIN outbound_txs t ON t.id = b.outbound_tx_id
WHERE b.network = @network AND b.state = 'paid' AND b.updated_at >= @since::timestamptz
ORDER BY b.updated_at, b.id;

-- name: ListSettledPayoutsSince :many
SELECT id, reference, state, naira_amount FROM payouts
WHERE state IN ('succeeded', 'failed', 'reversed') AND updated_at >= @since::timestamptz
  -- A payout that failed before the provider accepted it has no transfer.
  AND (state = 'succeeded' OR transfer_code IS NOT NULL)
ORDER BY updated_at, id;

-- name: ListPaidCheckoutsSince :many
SELECT id, reference, paid_amount FROM checkouts
WHERE state = 'paid' AND paid_at >= @since::timestamptz
ORDER BY paid_at, id;

-- name: ListAccountsBreakingRule :many
-- Accounts whose balance has the wrong sign for their rule. The ledger
-- refuses such postings; this checks that it did.
SELECT a.code, b.balance FROM ledger_accounts a JOIN ledger_balances b ON b.account_id = a.id
WHERE (a.balance_rule = 'non_negative' AND b.balance < 0)
   OR (a.balance_rule = 'non_positive' AND b.balance > 0)
ORDER BY a.code;

-- name: InsertReconciliationRun :exec
INSERT INTO reconciliation_runs (id, started_at, finished_at, as_of, differences, report)
VALUES (@id, @started_at, @finished_at, @as_of, @differences, @report);

-- name: LatestReconciliationRun :one
SELECT * FROM reconciliation_runs ORDER BY started_at DESC LIMIT 1;

-- name: LastCleanReconciliationStart :one
-- Record checks start from the last run with no differences; a run that
-- found one keeps checking everything since the last clean run.
SELECT coalesce(max(started_at), '1970-01-01T00:00:00Z'::timestamptz)::timestamptz AS since
FROM reconciliation_runs WHERE differences = 0;
