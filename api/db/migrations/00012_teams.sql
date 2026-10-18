-- Payer organisations, members and roles; contractor payout wallets;
-- organisation funding; batch runs with approval (docs/design/teams.md,
-- ADR-043). The ledger tables are unchanged.

-- +goose Up

CREATE TABLE payer_orgs (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    -- Runs whose total is above this (USDC minor units) need an approver
    -- other than their creator.
    approval_threshold minor_units NOT NULL DEFAULT 0 CHECK (approval_threshold >= 0),
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_members (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES payer_orgs (id),
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    roles      text[] NOT NULL CHECK (cardinality(roles) >= 1 AND roles <@ ARRAY['admin', 'maker', 'approver']),
    created_at timestamptz NOT NULL DEFAULT now(),
    removed_at timestamptz,
    UNIQUE (id, org_id)
);
CREATE INDEX org_members_org_idx ON org_members (org_id);

-- An API key belongs to a contractor or to an organisation member.
ALTER TABLE api_keys ALTER COLUMN contractor_id DROP NOT NULL;
ALTER TABLE api_keys ADD COLUMN member_id uuid REFERENCES org_members (id);
ALTER TABLE api_keys ADD CONSTRAINT api_keys_one_owner CHECK ((contractor_id IS NULL) <> (member_id IS NULL));

-- Where a contractor's on-chain payouts go, one wallet per network. A
-- changed wallet waits 24 hours before it can be paid (RISKS §2).
CREATE TABLE contractor_wallets (
    contractor_id        uuid NOT NULL REFERENCES contractors (id),
    network              text NOT NULL,
    address              text NOT NULL CHECK (address ~ '^0x[0-9a-f]{40}$'),
    added_at             timestamptz NOT NULL DEFAULT now(),
    payouts_allowed_from timestamptz NOT NULL,
    PRIMARY KEY (contractor_id, network)
);

-- Deposit addresses and payments belong to an invoice or, for funding, to
-- an organisation.
ALTER TABLE deposit_addresses ALTER COLUMN invoice_id DROP NOT NULL;
ALTER TABLE deposit_addresses ADD COLUMN org_id uuid REFERENCES payer_orgs (id);
ALTER TABLE deposit_addresses ADD CONSTRAINT deposit_addresses_one_owner CHECK ((invoice_id IS NULL) <> (org_id IS NULL));
ALTER TABLE deposit_addresses ADD CONSTRAINT deposit_addresses_org_network UNIQUE (org_id, network);

ALTER TABLE payments ALTER COLUMN invoice_id DROP NOT NULL;
ALTER TABLE payments ADD COLUMN org_id uuid REFERENCES payer_orgs (id);
ALTER TABLE payments ADD CONSTRAINT payments_one_owner CHECK ((invoice_id IS NULL) <> (org_id IS NULL));
CREATE INDEX payments_org_idx ON payments (org_id) WHERE org_id IS NOT NULL;

CREATE TABLE batches (
    id             uuid PRIMARY KEY,
    org_id         uuid NOT NULL REFERENCES payer_orgs (id),
    created_by     uuid NOT NULL,
    network        text NOT NULL,
    asset_code     text NOT NULL REFERENCES assets (code),
    total          minor_units NOT NULL CHECK (total > 0),
    state          text NOT NULL CHECK (state IN ('draft', 'ready', 'awaiting_approval', 'approved', 'sending',
                                                  'confirming', 'paid', 'failed', 'rejected', 'cancelled')),
    -- On-chain batch id: bytes32, used once by BatchPayout.
    chain_batch_id bytea NOT NULL UNIQUE CHECK (length(chain_batch_id) = 32),
    outbound_tx_id uuid REFERENCES outbound_txs (id),
    failure        text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (created_by, org_id) REFERENCES org_members (id, org_id),
    -- Lets approvals name the creator, so SQL itself refuses a self-approval.
    UNIQUE (id, created_by)
);
CREATE INDEX batches_org_idx ON batches (org_id, created_at DESC);
CREATE INDEX batches_active_idx ON batches (state) WHERE state IN ('sending', 'confirming');

CREATE TABLE batch_lines (
    batch_id       uuid NOT NULL REFERENCES batches (id),
    line_no        integer NOT NULL CHECK (line_no BETWEEN 1 AND 200),
    contractor_id  uuid NOT NULL REFERENCES contractors (id),
    -- The wallet as it was when the line was checked.
    wallet_address text CHECK (wallet_address ~ '^0x[0-9a-f]{40}$'),
    amount         minor_units NOT NULL CHECK (amount > 0),
    reference      text NOT NULL CHECK (length(reference) BETWEEN 1 AND 140),
    -- Why the line cannot be paid, if it cannot.
    problem        text,
    -- Vault.payout id: bytes32, used once on-chain (I16).
    payout_id      bytea NOT NULL UNIQUE CHECK (length(payout_id) = 32),
    tracking_code  text NOT NULL UNIQUE CHECK (tracking_code ~ '^WB(-[0-9A-HJKMNP-TV-Z]{4}){4}$'),
    PRIMARY KEY (batch_id, line_no)
);
CREATE INDEX batch_lines_dupe_idx ON batch_lines (contractor_id, reference);

CREATE TABLE approvals (
    batch_id    uuid NOT NULL,
    creator_id  uuid NOT NULL,
    approver_id uuid NOT NULL REFERENCES org_members (id),
    decision    text NOT NULL CHECK (decision IN ('approved', 'rejected')),
    decided_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (batch_id, approver_id),
    FOREIGN KEY (batch_id, creator_id) REFERENCES batches (id, created_by),
    -- Approver ≠ creator (I17), refused by the database itself.
    CONSTRAINT approvals_not_creator CHECK (approver_id <> creator_id)
);

CREATE TABLE batch_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id   uuid NOT NULL REFERENCES batches (id),
    type       text NOT NULL,
    from_state text NOT NULL,
    to_state   text NOT NULL,
    member_id  uuid REFERENCES org_members (id),
    data       jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX batch_events_batch_idx ON batch_events (batch_id, id);

-- +goose Down
DROP TABLE batch_events;
DROP TABLE approvals;
DROP TABLE batch_lines;
DROP TABLE batches;
DROP INDEX payments_org_idx;
ALTER TABLE payments DROP CONSTRAINT payments_one_owner;
ALTER TABLE payments DROP COLUMN org_id;
ALTER TABLE payments ALTER COLUMN invoice_id SET NOT NULL;
ALTER TABLE deposit_addresses DROP CONSTRAINT deposit_addresses_org_network;
ALTER TABLE deposit_addresses DROP CONSTRAINT deposit_addresses_one_owner;
ALTER TABLE deposit_addresses DROP COLUMN org_id;
ALTER TABLE deposit_addresses ALTER COLUMN invoice_id SET NOT NULL;
DROP TABLE contractor_wallets;
ALTER TABLE api_keys DROP CONSTRAINT api_keys_one_owner;
ALTER TABLE api_keys DROP COLUMN member_id;
ALTER TABLE api_keys ALTER COLUMN contractor_id SET NOT NULL;
DROP TABLE org_members;
DROP TABLE payer_orgs;
