-- Naira settlement. See ADR-041.

-- Bank accounts ------------------------------------------------------------

-- name: GetBankAccount :one
SELECT * FROM contractor_bank_accounts WHERE contractor_id = @contractor_id;

-- name: UpsertBankAccount :one
-- A new or changed account waits until payouts_allowed_from; saving the
-- same account again keeps its date and recipient.
INSERT INTO contractor_bank_accounts (contractor_id, bank_code, bank_name, account_number, account_name,
                                      verified_at, payouts_allowed_from)
VALUES (@contractor_id, @bank_code, @bank_name, @account_number, @account_name, @verified_at, @payouts_allowed_from)
ON CONFLICT (contractor_id) DO UPDATE SET
    bank_name = EXCLUDED.bank_name,
    account_name = EXCLUDED.account_name,
    verified_at = EXCLUDED.verified_at,
    payouts_allowed_from = CASE
        WHEN contractor_bank_accounts.bank_code = EXCLUDED.bank_code
         AND contractor_bank_accounts.account_number = EXCLUDED.account_number
        THEN contractor_bank_accounts.payouts_allowed_from
        ELSE EXCLUDED.payouts_allowed_from END,
    recipient_code = CASE
        WHEN contractor_bank_accounts.bank_code = EXCLUDED.bank_code
         AND contractor_bank_accounts.account_number = EXCLUDED.account_number
        THEN contractor_bank_accounts.recipient_code
        ELSE NULL END,
    bank_code = EXCLUDED.bank_code,
    account_number = EXCLUDED.account_number,
    updated_at = now()
RETURNING *;

-- Rate quotes ---------------------------------------------------------------

-- name: InsertRateQuote :exec
INSERT INTO rate_quotes (id, from_asset, to_asset, mid, rate, spread_bps, source, observed_at, quoted_at, expires_at)
VALUES (@id, @from_asset, @to_asset, @mid, @rate, @spread_bps, @source, @observed_at, @quoted_at, @expires_at);

-- name: GetRateQuote :one
SELECT * FROM rate_quotes WHERE id = @id;

-- name: GetInvoiceForContractor :one
SELECT i.*, c.legal_name AS contractor_name, c.verified_at AS contractor_verified_at
FROM invoices i JOIN contractors c ON c.id = i.contractor_id
WHERE i.id = @id AND i.contractor_id = @contractor_id;

-- Provider events -----------------------------------------------------------

-- name: InsertProviderEvent :one
-- A replay of an event already stored inserts nothing and returns no row.
INSERT INTO provider_events (provider, event_id, event_type, raw)
VALUES (@provider, @event_id, @event_type, @raw)
ON CONFLICT (provider, event_id) DO NOTHING
RETURNING id;
