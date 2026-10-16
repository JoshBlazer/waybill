-- API key management and outgoing webhooks (ADR-038, invariant I13).

-- +goose Up

ALTER TABLE api_keys
    ADD COLUMN name text NOT NULL DEFAULT 'Default key' CHECK (length(name) BETWEEN 1 AND 100),
    ADD COLUMN last_used_at timestamptz;

CREATE TABLE webhook_endpoints (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contractor_id uuid NOT NULL REFERENCES contractors (id),
    url           text NOT NULL CHECK (length(url) BETWEEN 1 AND 2000),
    events        text[] NOT NULL CHECK (cardinality(events) >= 1),
    description   text CHECK (length(description) <= 200),
    -- Readable because Waybill signs with it (ADR-038). Never logged and
    -- never returned after creation.
    secret        text NOT NULL CHECK (secret ~ '^whsec_test_[0-9A-Za-z]{32}$'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at   timestamptz
);
CREATE INDEX webhook_endpoints_contractor_idx ON webhook_endpoints (contractor_id) WHERE disabled_at IS NULL;

-- One row per event. The body is rendered at delivery time from the
-- invoice, with the state it had when the event was created.
CREATE TABLE webhook_events (
    id            text PRIMARY KEY CHECK (id ~ '^evt_[0-9A-Z]{26}$'),
    contractor_id uuid NOT NULL REFERENCES contractors (id),
    type          text NOT NULL,
    invoice_id    uuid NOT NULL REFERENCES invoices (id),
    invoice_state text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- One row per delivery attempt: the log contractors read.
CREATE TABLE webhook_deliveries (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    endpoint_id     uuid NOT NULL REFERENCES webhook_endpoints (id),
    event_id        text NOT NULL REFERENCES webhook_events (id),
    attempt         integer NOT NULL CHECK (attempt >= 1),
    outcome         text NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'refused')),
    response_status integer,
    latency_ms      integer,
    error           text,
    next_attempt_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (endpoint_id, event_id, attempt)
);
CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (endpoint_id, id DESC);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhook_events;
DROP TABLE webhook_endpoints;
ALTER TABLE api_keys DROP COLUMN last_used_at, DROP COLUMN name;
