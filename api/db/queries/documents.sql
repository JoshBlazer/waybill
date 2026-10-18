-- Invoices, receipts and the accountant export (docs/design/exports.md).
-- Read-only.

-- name: ListFinalPaymentsForInvoice :many
SELECT network, tx_hash, amount, final_at FROM payments
WHERE invoice_id = @invoice_id AND state = 'final'
ORDER BY final_at, id;

-- name: GetPaidCheckoutForInvoice :one
SELECT reference, paid_amount, paid_at FROM checkouts
WHERE invoice_id = @invoice_id AND state = 'paid';

-- name: InvoiceSettledAt :one
SELECT coalesce(min(created_at), 'epoch'::timestamptz)::timestamptz AS settled_at
FROM invoice_events WHERE invoice_id = @invoice_id AND to_state = 'settled';

-- name: ListExportPostings :many
-- Every posting on the given accounts in [from, to), oldest first.
SELECT p.id AS posting_id, e.id AS entry_id, e.created_at, e.kind, e.ref_type, e.ref_id,
       e.reverses_entry_id, a.code AS account_code, p.asset_code, p.amount
FROM postings p
JOIN journal_entries e ON e.id = p.entry_id
JOIN ledger_accounts a ON a.id = p.account_id
WHERE a.code = ANY(@codes::text[]) AND e.created_at >= @from_time AND e.created_at < @to_time
ORDER BY e.created_at, e.id, p.id;

-- name: SumPostingsBefore :many
-- Opening balances: the sum per account of everything before from.
SELECT a.code AS account_code, coalesce(sum(p.amount), 0)::minor_units AS total
FROM ledger_accounts a
LEFT JOIN postings p ON p.account_id = a.id
LEFT JOIN journal_entries e ON e.id = p.entry_id
WHERE a.code = ANY(@codes::text[]) AND (e.id IS NULL OR e.created_at < @from_time)
GROUP BY a.code;

-- name: PaymentRefs :many
SELECT p.id, p.tx_hash, i.tracking_code
FROM payments p LEFT JOIN invoices i ON i.id = p.invoice_id
WHERE p.id = ANY(@ids::uuid[]);

-- name: PayoutRefs :many
SELECT po.id, po.reference, i.tracking_code
FROM payouts po LEFT JOIN invoices i ON i.id = po.invoice_id
WHERE po.id = ANY(@ids::uuid[]);

-- name: CheckoutRefs :many
SELECT c.id, c.reference, i.tracking_code
FROM checkouts c JOIN invoices i ON i.id = c.invoice_id
WHERE c.id = ANY(@ids::uuid[]);

-- name: BatchRefs :many
SELECT b.id, t.mined_hash
FROM batches b LEFT JOIN outbound_txs t ON t.id = b.outbound_tx_id
WHERE b.id = ANY(@ids::uuid[]);

-- name: ConversionRefs :many
SELECT c.id, po.reference
FROM conversions c LEFT JOIN payouts po ON po.conversion_id = c.id
WHERE c.id = ANY(@ids::uuid[]);
