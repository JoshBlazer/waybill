-- Payer organisations, members, funding and contractor wallets
-- (docs/design/teams.md).

-- name: UpsertOrg :one
INSERT INTO payer_orgs (id, name, approval_threshold)
VALUES (@id, @name, @approval_threshold)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name
RETURNING *;

-- name: GetOrg :one
SELECT * FROM payer_orgs WHERE id = @id;

-- name: SetOrgThreshold :one
UPDATE payer_orgs SET approval_threshold = @approval_threshold WHERE id = @id
RETURNING *;

-- name: UpsertMember :one
INSERT INTO org_members (id, org_id, name, roles)
VALUES (@id, @org_id, @name, @roles)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, roles = EXCLUDED.roles
RETURNING *;

-- name: InsertMember :one
INSERT INTO org_members (org_id, name, roles)
VALUES (@org_id, @name, @roles)
RETURNING *;

-- name: GetMember :one
SELECT * FROM org_members WHERE id = @id AND org_id = @org_id;

-- name: ListMembers :many
SELECT * FROM org_members WHERE org_id = @org_id ORDER BY created_at, id;

-- name: LockOrgMembers :many
-- Serialises role changes in one organisation, so two concurrent changes
-- cannot each leave the other as the last admin and remove both.
SELECT id, roles, removed_at FROM org_members WHERE org_id = @org_id ORDER BY id FOR UPDATE;

-- name: SetMemberRoles :one
UPDATE org_members SET roles = @roles WHERE id = @id AND org_id = @org_id AND removed_at IS NULL
RETURNING *;

-- name: RemoveMember :one
UPDATE org_members SET removed_at = coalesce(removed_at, now()) WHERE id = @id AND org_id = @org_id
RETURNING *;

-- name: InsertMemberAPIKey :exec
INSERT INTO api_keys (member_id, prefix, secret_sha256, name)
VALUES (@member_id::uuid, @prefix, @secret_sha256, @name)
ON CONFLICT (prefix) DO NOTHING;

-- name: InsertOrgDepositAddress :exec
INSERT INTO deposit_addresses (network, address, org_id, salt)
VALUES (@network, @address, @org_id::uuid, @salt)
ON CONFLICT DO NOTHING;

-- name: ListOrgDepositAddresses :many
SELECT * FROM deposit_addresses WHERE org_id = @org_id::uuid ORDER BY network;

-- name: GetBalanceByCode :one
-- Zero when the account does not exist yet.
SELECT coalesce((SELECT b.balance FROM ledger_balances b
                 JOIN ledger_accounts a ON a.id = b.account_id
                 WHERE a.code = @code), 0)::minor_units AS balance;
