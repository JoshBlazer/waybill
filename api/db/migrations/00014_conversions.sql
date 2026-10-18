-- Contractor balance conversion (docs/design/conversion.md, ADR-044). Not
-- a ledger table: the conversion's ledger entry goes through ledger.Post.

-- +goose Up

CREATE TABLE conversions (
    id            uuid PRIMARY KEY,
    contractor_id uuid NOT NULL REFERENCES contractors (id),
    usdc_amount   minor_units NOT NULL CHECK (usdc_amount >= 1000000), -- at least 1 USDC
    naira_amount  minor_units NOT NULL CHECK (naira_amount > 0),       -- kobo at the quoted rate
    rate_quote_id uuid NOT NULL REFERENCES rate_quotes (id),
    -- A quoted conversion whose quote has expired is shown as expired; it
    -- can never be accepted. Nothing needs to write that down.
    state         text NOT NULL CHECK (state IN ('quoted', 'accepted')),
    accepted_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'accepted') = (accepted_at IS NOT NULL))
);
CREATE INDEX conversions_contractor_idx ON conversions (contractor_id, created_at DESC);

-- A payout comes from a settled invoice or from an accepted conversion.
ALTER TABLE payouts ALTER COLUMN invoice_id DROP NOT NULL;
ALTER TABLE payouts ADD COLUMN conversion_id uuid UNIQUE REFERENCES conversions (id);
ALTER TABLE payouts ADD CONSTRAINT payouts_one_source CHECK ((invoice_id IS NULL) <> (conversion_id IS NULL));

-- Conversion payouts have no public tracking page, so their events carry
-- no invoice and notify nobody.
ALTER TABLE payout_events ALTER COLUMN invoice_id DROP NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION payout_events_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.invoice_id IS NOT NULL THEN
        PERFORM pg_notify('invoice_events', NEW.invoice_id::text);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION payout_events_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('invoice_events', NEW.invoice_id::text);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
DELETE FROM payout_events WHERE invoice_id IS NULL;
ALTER TABLE payout_events ALTER COLUMN invoice_id SET NOT NULL;
ALTER TABLE payouts DROP CONSTRAINT payouts_one_source;
DELETE FROM payouts WHERE conversion_id IS NOT NULL;
ALTER TABLE payouts DROP COLUMN conversion_id;
ALTER TABLE payouts ALTER COLUMN invoice_id SET NOT NULL;
DROP TABLE conversions;
