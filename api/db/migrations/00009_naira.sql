-- Naira settlement, part 1: verified bank accounts, locked rate quotes and
-- stored provider events (ADR-039, ADR-040, ADR-041).

-- +goose Up

-- One payout account per contractor, saved only after the bank's name for
-- it matched the contractor's legal name.
CREATE TABLE contractor_bank_accounts (
    contractor_id        uuid PRIMARY KEY REFERENCES contractors (id),
    bank_code            text NOT NULL CHECK (bank_code ~ '^[0-9]{3,6}$'),
    bank_name            text NOT NULL,
    -- Needed to create the provider's transfer recipient. Shown back only
    -- as its last four digits.
    account_number       text NOT NULL CHECK (account_number ~ '^[0-9]{10}$'),
    account_name         text NOT NULL CHECK (length(account_name) BETWEEN 1 AND 200),
    verified_at          timestamptz NOT NULL,
    -- Payouts to a changed account wait (account takeover, RISKS §2).
    payouts_allowed_from timestamptz NOT NULL,
    -- The provider's recipient for this account; reset when it changes.
    recipient_code       text,
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- A rate Waybill committed to. Rates are minor units of the target asset
-- per one whole source unit (kobo per USDC).
CREATE TABLE rate_quotes (
    id          uuid PRIMARY KEY,
    from_asset  text NOT NULL REFERENCES assets (code),
    to_asset    text NOT NULL REFERENCES assets (code),
    mid         minor_units NOT NULL CHECK (mid > 0),
    rate        minor_units NOT NULL CHECK (rate > 0 AND rate <= mid),
    spread_bps  integer NOT NULL CHECK (spread_bps BETWEEN 0 AND 9999),
    source      text NOT NULL,
    observed_at timestamptz NOT NULL,
    quoted_at   timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL CHECK (expires_at > quoted_at)
);

ALTER TABLE invoices ADD COLUMN rate_quote_id uuid REFERENCES rate_quotes (id);
-- A naira invoice always has a quote; a hold invoice never does.
ALTER TABLE invoices ADD CONSTRAINT invoices_naira_has_quote
    CHECK ((payout_mode = 'naira') = (rate_quote_id IS NOT NULL));

-- Every callback a provider sent, stored raw before anything is done with
-- it, and processed once (I14). Paystack bodies carry no event id, so the
-- id is the event name and the provider's id for the object.
CREATE TABLE provider_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider     text NOT NULL CHECK (provider IN ('paystack')),
    event_id     text NOT NULL CHECK (length(event_id) BETWEEN 1 AND 200),
    event_type   text NOT NULL,
    raw          bytea NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    UNIQUE (provider, event_id)
);

-- +goose Down
DROP TABLE provider_events;
ALTER TABLE invoices DROP CONSTRAINT invoices_naira_has_quote;
ALTER TABLE invoices DROP COLUMN rate_quote_id;
DROP TABLE rate_quotes;
DROP TABLE contractor_bank_accounts;
