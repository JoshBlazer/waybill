-- Outbox. See docs/ARCHITECTURE.md §7.

-- name: EnqueueOutbox :one
-- With a dedupe key already used, nothing is inserted and no row returns.
INSERT INTO outbox (topic, payload, dedupe_key, available_at)
VALUES (@topic, @payload, sqlc.narg(dedupe_key), coalesce(sqlc.narg(available_at)::timestamptz, now()))
ON CONFLICT (dedupe_key) DO NOTHING
RETURNING id;

-- name: LeaseOutbox :many
-- Leases up to max ready jobs for owner. A job is ready when it is due, not
-- finished, and not leased (or its lease expired: the worker crashed).
-- SKIP LOCKED lets concurrent workers take disjoint jobs without waiting.
UPDATE outbox
SET lease_owner = @owner,
    lease_expires_at = statement_timestamp() + sqlc.arg(lease_ms)::bigint * interval '1 millisecond',
    attempts = attempts + 1
WHERE id IN (
    SELECT o.id FROM outbox o
    WHERE o.done_at IS NULL AND o.dead_at IS NULL
      AND o.available_at <= statement_timestamp()
      AND (o.lease_expires_at IS NULL OR o.lease_expires_at <= statement_timestamp())
    ORDER BY o.available_at, o.id
    LIMIT sqlc.arg(max)::integer
    FOR UPDATE SKIP LOCKED
)
RETURNING id, topic, payload, attempts;

-- name: CompleteOutbox :execrows
-- Only the current lease holder can finish a job.
UPDATE outbox SET done_at = now(), lease_owner = NULL, lease_expires_at = NULL, last_error = NULL
WHERE id = @id AND lease_owner = @owner AND done_at IS NULL;

-- name: RetryOutbox :execrows
UPDATE outbox
SET available_at = statement_timestamp() + sqlc.arg(delay_ms)::bigint * interval '1 millisecond',
    lease_owner = NULL, lease_expires_at = NULL, last_error = @last_error
WHERE id = @id AND lease_owner = @owner AND done_at IS NULL;

-- name: KillOutbox :execrows
UPDATE outbox SET dead_at = now(), lease_owner = NULL, lease_expires_at = NULL, last_error = @last_error
WHERE id = @id AND lease_owner = @owner AND done_at IS NULL;
