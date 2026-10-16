-- Card and bank checkout. See ADR-041.

-- name: GetOpenCheckout :one
SELECT * FROM checkouts WHERE invoice_id = @invoice_id AND state = 'open';

-- name: InsertCheckout :one
-- Another open checkout for the invoice wins the race: no row is returned.
INSERT INTO checkouts (id, invoice_id, reference, amount, state, expires_at)
VALUES (@id, @invoice_id, @reference, @amount, 'open', @expires_at)
ON CONFLICT (invoice_id) WHERE state = 'open' DO NOTHING
RETURNING *;

-- name: SetCheckoutURL :one
UPDATE checkouts SET authorization_url = @authorization_url, updated_at = now()
WHERE id = @id RETURNING *;

-- name: GetCheckoutByReference :one
SELECT * FROM checkouts WHERE reference = @reference;

-- name: LockCheckout :one
SELECT * FROM checkouts WHERE id = @id FOR UPDATE;

-- name: ListOpenCheckouts :many
SELECT * FROM checkouts WHERE state = 'open' ORDER BY created_at LIMIT 100;

-- name: MarkCheckoutPaid :exec
UPDATE checkouts
SET state = 'paid', paid_at = @paid_at, paid_amount = @paid_amount,
    provider_transaction_id = @provider_transaction_id, updated_at = now()
WHERE id = @id AND state = 'open';

-- name: CloseCheckout :exec
-- failed or expired; only from open.
UPDATE checkouts SET state = @state, failure = @failure, updated_at = now()
WHERE id = @id AND state = 'open';

-- name: ListUnprocessedProviderEvents :many
SELECT * FROM provider_events
WHERE provider = @provider AND event_type = @event_type AND processed_at IS NULL
ORDER BY id LIMIT 100;

-- name: MarkProviderEventProcessed :exec
UPDATE provider_events SET processed_at = now() WHERE id = @id AND processed_at IS NULL;
