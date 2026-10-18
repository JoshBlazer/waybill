-- Contractor balance conversion (docs/design/conversion.md).

-- name: InsertConversion :one
INSERT INTO conversions (id, contractor_id, usdc_amount, naira_amount, rate_quote_id, state)
VALUES (@id, @contractor_id, @usdc_amount, @naira_amount, @rate_quote_id, 'quoted')
RETURNING *;

-- name: GetConversion :one
SELECT c.*, q.rate, q.expires_at
FROM conversions c JOIN rate_quotes q ON q.id = c.rate_quote_id
WHERE c.id = @id AND c.contractor_id = @contractor_id;

-- name: LockConversion :one
SELECT * FROM conversions WHERE id = @id AND contractor_id = @contractor_id FOR UPDATE;

-- name: MarkConversionAccepted :exec
UPDATE conversions SET state = 'accepted', accepted_at = @now::timestamptz WHERE id = @id;

-- name: ListConversions :many
SELECT c.*, q.rate, q.expires_at
FROM conversions c JOIN rate_quotes q ON q.id = c.rate_quote_id
WHERE c.contractor_id = @contractor_id
ORDER BY c.created_at DESC
LIMIT @max_rows;

-- name: InsertConversionPayout :one
-- The naira is fixed at acceptance, so the payout starts converted.
INSERT INTO payouts (id, conversion_id, contractor_id, state, source_asset, source_amount, naira_amount, rate_quote_id, reference)
VALUES (@id, @conversion_id, @contractor_id, 'converted', 'USDC', @source_amount, @naira_amount, @rate_quote_id, @reference)
RETURNING *;

-- name: GetPayoutByConversion :one
SELECT * FROM payouts WHERE conversion_id = @conversion_id;
