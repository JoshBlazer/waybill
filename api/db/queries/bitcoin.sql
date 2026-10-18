-- Bitcoin receive (docs/design/stage5.md §2).

-- name: NextBTCIndex :one
-- Takes the next derivation index under a row lock, so two invoices never
-- get the same address.
INSERT INTO btc_address_counters (network, next_index) VALUES (@network, 1)
ON CONFLICT (network) DO UPDATE SET next_index = btc_address_counters.next_index + 1
RETURNING next_index - 1 AS index;

-- name: InsertBTCAddress :exec
INSERT INTO btc_addresses (address, network, invoice_id, derivation_index)
VALUES (@address, @network, @invoice_id, @derivation_index);

-- name: ListBTCAddressesByInvoice :many
SELECT * FROM btc_addresses WHERE invoice_id = @invoice_id ORDER BY network;

-- name: ListBTCAddresses :many
SELECT address, invoice_id FROM btc_addresses WHERE network = @network;

-- name: InsertBTCPayment :one
-- Nothing on conflict: the output is already recorded (I9).
INSERT INTO btc_payments (network, txid, vout, block_height, block_hash, address, invoice_id, amount)
VALUES (@network, @txid, @vout, @block_height, @block_hash, @address, @invoice_id, @amount)
ON CONFLICT (network, txid, vout) WHERE state <> 'reorged' DO NOTHING
RETURNING *;

-- name: ListOpenBTCPayments :many
SELECT * FROM btc_payments WHERE network = @network AND state IN ('detected', 'confirmed') ORDER BY block_height, txid, vout;

-- name: MarkBTCPaymentConfirmed :exec
UPDATE btc_payments SET state = 'confirmed', confirmed_at = now() WHERE id = @id AND state = 'detected';

-- name: MarkBTCPaymentFinal :exec
UPDATE btc_payments SET state = 'final', final_at = now() WHERE id = @id AND state = 'confirmed';

-- name: MarkBTCPaymentReorged :execrows
UPDATE btc_payments SET state = 'reorged' WHERE id = @id AND state IN ('detected', 'confirmed');

-- name: SumBTCPaymentsByState :one
SELECT coalesce(sum(amount), 0)::minor_units AS total
FROM btc_payments WHERE invoice_id = @invoice_id AND state = ANY(@states::text[]);

-- name: ListLiveBTCPaymentsByInvoice :many
SELECT * FROM btc_payments WHERE invoice_id = @invoice_id AND state <> 'reorged' ORDER BY block_height, txid, vout;

-- name: GetBTCCursor :one
SELECT next_height FROM btc_cursors WHERE network = @network;

-- name: SetBTCCursor :exec
INSERT INTO btc_cursors (network, next_height) VALUES (@network, @next_height)
ON CONFLICT (network) DO UPDATE SET next_height = EXCLUDED.next_height;

-- name: PutBTCBlock :exec
INSERT INTO btc_blocks (network, height, hash) VALUES (@network, @height, @hash)
ON CONFLICT (network, height) DO UPDATE SET hash = EXCLUDED.hash;

-- name: GetBTCTip :one
SELECT height, hash FROM btc_blocks WHERE network = @network ORDER BY height DESC LIMIT 1;

-- name: ListBTCBlocksBelow :many
SELECT height, hash FROM btc_blocks WHERE network = @network AND height < @height ORDER BY height DESC LIMIT 200;

-- name: DeleteBTCBlocksFrom :exec
DELETE FROM btc_blocks WHERE network = @network AND height >= @height;

-- name: PruneBTCBlocks :exec
DELETE FROM btc_blocks WHERE network = @network AND height < @height;

-- name: ListBTCAddressesAll :many
SELECT address FROM btc_addresses WHERE network = @network ORDER BY address;

-- name: ListDetectedBTCPaymentsUpTo :many
SELECT id, amount FROM btc_payments WHERE network = @network AND state = 'detected' AND block_height <= @height::bigint;

-- name: ListBookedBTCPaymentsAbove :many
SELECT id, amount FROM btc_payments WHERE network = @network AND state IN ('confirmed', 'final') AND block_height > @height::bigint;
