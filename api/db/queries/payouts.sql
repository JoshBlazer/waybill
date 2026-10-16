-- Naira payouts. See docs/design/payout.md.

-- name: ListSettledNairaWithoutPayout :many
-- Settled naira invoices that have no payout yet, with what arrived: the
-- final on-chain payments, or a paid card checkout.
SELECT i.id, i.contractor_id,
       coalesce((SELECT sum(p.amount) FROM payments p WHERE p.invoice_id = i.id AND p.state = 'final'), 0)::minor_units AS chain_amount,
       coalesce((SELECT c.paid_amount FROM checkouts c WHERE c.invoice_id = i.id AND c.state = 'paid'), 0)::minor_units AS card_amount
FROM invoices i
WHERE i.state = 'settled' AND i.payout_mode = 'naira'
  AND NOT EXISTS (SELECT 1 FROM payouts po WHERE po.invoice_id = i.id)
ORDER BY i.updated_at
LIMIT 100;

-- name: InsertPayout :one
INSERT INTO payouts (id, invoice_id, contractor_id, state, source_asset, source_amount, reference)
VALUES (@id, @invoice_id, @contractor_id, 'queued', @source_asset, @source_amount, @reference)
ON CONFLICT (invoice_id) DO NOTHING
RETURNING *;

-- name: ListActivePayouts :many
SELECT * FROM payouts
WHERE state IN ('queued', 'converted', 'submitting', 'unknown', 'submitted')
ORDER BY created_at
LIMIT 100;

-- name: LockPayout :one
SELECT * FROM payouts WHERE id = @id FOR UPDATE;

-- name: GetPayoutByInvoice :one
SELECT * FROM payouts WHERE invoice_id = @invoice_id;

-- name: GetPayoutByReference :one
SELECT * FROM payouts WHERE reference = @reference;

-- name: SetPayoutState :exec
UPDATE payouts SET state = @state, failure = sqlc.narg(failure), updated_at = now() WHERE id = @id;

-- name: SetPayoutConversion :exec
UPDATE payouts SET naira_amount = @naira_amount, rate_quote_id = sqlc.narg(rate_quote_id), updated_at = now()
WHERE id = @id;

-- name: SetPayoutTransferCode :exec
UPDATE payouts SET transfer_code = @transfer_code, updated_at = now() WHERE id = @id;

-- name: InsertPayoutEvent :exec
INSERT INTO payout_events (payout_id, invoice_id, type, from_state, to_state, data)
VALUES (@payout_id, @invoice_id, @type, @from_state, @to_state, @data);

-- name: ListPayoutEvents :many
SELECT * FROM payout_events WHERE invoice_id = @invoice_id ORDER BY id;

-- name: FirstPaymentDetectedAt :one
-- When money first arrived for the invoice, by chain or by card.
SELECT least(
    (SELECT min(p.detected_at) FROM payments p WHERE p.invoice_id = sqlc.arg(invoice)::uuid AND p.state <> 'reorged'),
    (SELECT min(c.paid_at) FROM checkouts c WHERE c.invoice_id = sqlc.arg(invoice)::uuid AND c.state = 'paid')
)::timestamptz AS first_at;

-- name: SetBankRecipient :exec
UPDATE contractor_bank_accounts SET recipient_code = @recipient_code WHERE contractor_id = @contractor_id;
