-- Ledger queries. See docs/design/ledger.md.

-- name: InsertAccount :one
-- Creates an account, or returns the existing one with the same code. The
-- caller compares the returned row with what it asked for.
INSERT INTO ledger_accounts (code, asset_code, kind, balance_rule)
VALUES (@code, @asset_code, @kind, @balance_rule)
ON CONFLICT (code) DO NOTHING
RETURNING id, code, asset_code, kind, balance_rule;

-- name: GetAccountByCode :one
SELECT id, code, asset_code, kind, balance_rule FROM ledger_accounts WHERE code = @code;

-- name: GetAccountsByIDs :many
-- Ordered by id: callers that lock accounts must always lock in this order.
SELECT id, code, asset_code, kind, balance_rule
FROM ledger_accounts WHERE id = ANY(@ids::bigint[]) ORDER BY id;

-- name: GetBalance :one
SELECT balance FROM ledger_balances WHERE account_id = @account_id;

-- name: InsertJournalEntry :one
INSERT INTO journal_entries (idempotency_key, kind, ref_type, ref_id, memo, reverses_entry_id)
VALUES (@idempotency_key, @kind, @ref_type, @ref_id, @memo, sqlc.narg(reverses_entry_id))
RETURNING id, created_at;

-- name: InsertPosting :exec
INSERT INTO postings (entry_id, account_id, asset_code, amount)
VALUES (@entry_id, @account_id, @asset_code, @amount);

-- name: GetJournalEntry :one
SELECT id, idempotency_key, kind, ref_type, ref_id, memo, reverses_entry_id, created_at
FROM journal_entries WHERE id = @id;

-- name: GetJournalEntryByIdempotencyKey :one
SELECT id, idempotency_key, kind, ref_type, ref_id, memo, reverses_entry_id, created_at
FROM journal_entries WHERE idempotency_key = @idempotency_key;

-- name: GetReversalOf :one
SELECT id FROM journal_entries WHERE reverses_entry_id = @entry_id;

-- name: ListPostingsByEntry :many
SELECT id, entry_id, account_id, asset_code, amount
FROM postings WHERE entry_id = @entry_id ORDER BY account_id, id;

-- name: TrialBalance :many
-- Sum of all postings per asset. Every row must be zero.
SELECT asset_code, sum(amount)::minor_units AS total FROM postings GROUP BY asset_code ORDER BY asset_code;

-- name: BalanceCacheMismatches :many
-- Accounts whose cached balance differs from the sum of their postings.
-- Must always return no rows.
SELECT b.account_id, b.balance, coalesce(sum(p.amount), 0)::minor_units AS posted
FROM ledger_balances b
LEFT JOIN postings p ON p.account_id = b.account_id
GROUP BY b.account_id, b.balance
HAVING b.balance <> coalesce(sum(p.amount), 0);
