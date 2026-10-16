-- Naira payouts (docs/design/payout.md, ARCHITECTURE §5.2).

-- +goose Up

CREATE TABLE payouts (
    id            uuid PRIMARY KEY,
    invoice_id    uuid NOT NULL UNIQUE REFERENCES invoices (id),
    contractor_id uuid NOT NULL REFERENCES contractors (id),
    state         text NOT NULL CHECK (state IN ('queued', 'converted', 'held', 'submitting', 'submitted',
                                                 'unknown', 'succeeded', 'failed', 'reversed', 'cancelled')),
    -- What arrived: final on-chain payments (USDC) or a card payment (NGN).
    source_asset  text NOT NULL REFERENCES assets (code),
    source_amount minor_units NOT NULL CHECK (source_amount > 0),
    -- Kobo to send; set at conversion.
    naira_amount  minor_units NOT NULL DEFAULT 0 CHECK (naira_amount >= 0),
    -- The quote used: the invoice's, or a new one for a late payment.
    rate_quote_id uuid REFERENCES rate_quotes (id),
    -- Sent to the provider on every attempt, so it can never pay twice.
    reference     text NOT NULL UNIQUE CHECK (reference ~ '^[a-z0-9_-]{16,100}$'),
    transfer_code text,
    failure       text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (state IN ('queued', 'held', 'cancelled') OR naira_amount > 0)
);
CREATE INDEX payouts_active_idx ON payouts (state) WHERE state IN ('queued', 'converted', 'submitting', 'unknown', 'submitted');

CREATE TABLE payout_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payout_id  uuid NOT NULL REFERENCES payouts (id),
    invoice_id uuid NOT NULL REFERENCES invoices (id),
    type       text NOT NULL,
    from_state text NOT NULL,
    to_state   text NOT NULL,
    data       jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payout_events_invoice_idx ON payout_events (invoice_id, id);

-- The tracking page listens on the invoice_events channel: a payout step
-- notifies it the same way, at commit.
-- +goose StatementBegin
CREATE FUNCTION payout_events_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('invoice_events', NEW.invoice_id::text);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER payout_events_notify AFTER INSERT ON payout_events
    FOR EACH ROW EXECUTE FUNCTION payout_events_notify();

-- +goose Down
DROP TRIGGER payout_events_notify ON payout_events;
DROP FUNCTION payout_events_notify();
DROP TABLE payout_events;
DROP TABLE payouts;
