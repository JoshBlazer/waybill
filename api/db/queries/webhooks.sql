-- API key management and outgoing webhooks. See ADR-038.

-- API keys ---------------------------------------------------------------

-- name: CreateAPIKey :one
INSERT INTO api_keys (contractor_id, prefix, secret_sha256, name)
VALUES (@contractor_id, @prefix, @secret_sha256, @name)
RETURNING id, name, prefix, created_at, last_used_at, revoked_at;

-- name: ListAPIKeys :many
SELECT id, name, prefix, created_at, last_used_at, revoked_at
FROM api_keys WHERE contractor_id = @contractor_id
ORDER BY created_at DESC, id;

-- name: LockContractorAPIKeys :many
-- Serialises revocations for one contractor, so two concurrent revokes
-- cannot each see the other key as "still active" and remove both.
SELECT id, revoked_at FROM api_keys WHERE contractor_id = @contractor_id ORDER BY id FOR UPDATE;

-- name: RevokeAPIKey :exec
UPDATE api_keys SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL;

-- name: TouchAPIKey :exec
-- Records use at most once a minute per key, so authentication does not
-- write on every request.
UPDATE api_keys SET last_used_at = now()
WHERE prefix = @prefix AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- Webhook endpoints ------------------------------------------------------

-- name: CreateWebhookEndpoint :one
INSERT INTO webhook_endpoints (contractor_id, url, events, description, secret)
VALUES (@contractor_id, @url, @events, sqlc.narg(description), @secret)
RETURNING *;

-- name: ListWebhookEndpoints :many
SELECT * FROM webhook_endpoints WHERE contractor_id = @contractor_id ORDER BY created_at DESC, id;

-- name: GetWebhookEndpoint :one
SELECT * FROM webhook_endpoints WHERE id = @id;

-- name: DisableWebhookEndpoint :execrows
UPDATE webhook_endpoints SET disabled_at = now()
WHERE id = @id AND contractor_id = @contractor_id AND disabled_at IS NULL;

-- name: ListSubscribedEndpoints :many
SELECT id FROM webhook_endpoints
WHERE contractor_id = @contractor_id AND disabled_at IS NULL AND @type::text = ANY (events)
ORDER BY id;

-- Events and deliveries ---------------------------------------------------

-- name: InsertWebhookEvent :exec
INSERT INTO webhook_events (id, contractor_id, type, invoice_id, invoice_state)
VALUES (@id, @contractor_id, @type, @invoice_id, @invoice_state);

-- name: GetWebhookEvent :one
SELECT * FROM webhook_events WHERE id = @id;

-- name: InsertWebhookDelivery :exec
INSERT INTO webhook_deliveries (endpoint_id, event_id, attempt, outcome, response_status, latency_ms, error, next_attempt_at)
VALUES (@endpoint_id, @event_id, @attempt, @outcome, sqlc.narg(response_status), sqlc.narg(latency_ms), sqlc.narg(error), sqlc.narg(next_attempt_at))
ON CONFLICT (endpoint_id, event_id, attempt) DO NOTHING;

-- name: ListWebhookDeliveries :many
SELECT d.id, d.event_id, e.type AS event_type, d.attempt, d.outcome, d.response_status,
       d.latency_ms, d.error, d.next_attempt_at, d.created_at
FROM webhook_deliveries d JOIN webhook_events e ON e.id = d.event_id
WHERE d.endpoint_id = @endpoint_id AND d.id < @before
ORDER BY d.id DESC
LIMIT @max;

-- name: CancelWebhookJobs :exec
-- Pending deliveries to a disabled endpoint stop. Outbox jobs are not
-- deleted; they are marked done, so the history stays.
UPDATE outbox SET done_at = now(), lease_owner = NULL, lease_expires_at = NULL,
       last_error = 'endpoint disabled'
WHERE topic = 'webhook.deliver' AND done_at IS NULL AND dead_at IS NULL
  AND payload->>'endpointId' = @endpoint_id::text;
