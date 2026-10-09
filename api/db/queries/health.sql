-- name: Ping :one
-- Confirms the database answers queries, not only that a connection opens.
SELECT 1::int AS ok;
