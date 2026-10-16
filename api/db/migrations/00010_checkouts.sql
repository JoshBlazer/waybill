-- Card and bank pay-in through Paystack test checkout (ADR-041).

-- +goose Up

CREATE TABLE checkouts (
    id                      uuid PRIMARY KEY,
    invoice_id              uuid NOT NULL REFERENCES invoices (id),
    -- Sent to Paystack and used to verify: lowercase, 16 to 100 characters.
    reference               text NOT NULL UNIQUE CHECK (reference ~ '^[a-z0-9_-]{16,100}$'),
    amount                  minor_units NOT NULL CHECK (amount > 0), -- kobo, at the invoice's locked rate
    authorization_url       text CHECK (authorization_url ~ '^https://'),
    state                   text NOT NULL CHECK (state IN ('open', 'paid', 'failed', 'expired')),
    failure                 text,
    provider_transaction_id text,
    paid_amount             minor_units NOT NULL DEFAULT 0 CHECK (paid_amount >= 0), -- kobo actually paid
    created_at              timestamptz NOT NULL DEFAULT now(),
    expires_at              timestamptz NOT NULL,
    paid_at                 timestamptz,
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'paid') = (paid_at IS NOT NULL AND paid_amount > 0))
);
-- One open checkout per invoice, and an invoice is paid by card at most once.
CREATE UNIQUE INDEX checkouts_one_open ON checkouts (invoice_id) WHERE state = 'open';
CREATE UNIQUE INDEX checkouts_one_paid ON checkouts (invoice_id) WHERE state = 'paid';

-- +goose Down
DROP TABLE checkouts;
