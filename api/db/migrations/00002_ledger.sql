-- Double-entry, append-only, multi-asset ledger.
-- Design: docs/design/ledger.md. Invariants I2-I5: docs/ARCHITECTURE.md §6.
--
-- What the database guarantees on its own, whatever the application does:
--   * amounts are whole minor units (domain minor_units, no silent rounding)
--   * a posting's asset matches its account's asset (composite foreign key)
--   * every entry has at least two postings and sums to zero per asset (at commit)
--   * postings can only be added to an entry in the transaction that created it
--   * entries and postings are never updated, deleted or truncated
--   * cached balances equal the sum of postings and obey each account's rule
--   * a reversal exactly negates its original, and an entry is reversed at most once

-- +goose Up

-- Whole minor units. Unconstrained numeric, because numeric(78,0) would
-- silently round 1.5 to 2 before any CHECK could see it.
CREATE DOMAIN minor_units AS numeric
    CHECK (scale(VALUE) = 0 AND abs(VALUE) < 1e78);

CREATE TABLE assets (
    code  text PRIMARY KEY CHECK (code ~ '^[A-Z][A-Z0-9]{1,11}$'),
    scale smallint NOT NULL CHECK (scale BETWEEN 0 AND 18)
);

INSERT INTO assets (code, scale) VALUES
    ('USDC', 6), ('USDT', 6), ('NGN', 2), ('BTC', 8), ('ETH', 18);

CREATE TABLE ledger_accounts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code         text NOT NULL UNIQUE CHECK (code ~ '^[a-z][a-z0-9_]*(:[A-Za-z0-9_.-]+)+$'),
    asset_code   text NOT NULL REFERENCES assets (code),
    kind         text NOT NULL CHECK (kind IN ('asset', 'liability', 'equity', 'revenue', 'expense')),
    balance_rule text NOT NULL CHECK (balance_rule IN ('non_negative', 'non_positive', 'any')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Targets for composite foreign keys below.
    UNIQUE (id, asset_code),
    UNIQUE (id, balance_rule)
);

CREATE TABLE journal_entries (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    idempotency_key   text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    kind              text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$'),
    ref_type          text NOT NULL DEFAULT '',
    ref_id            text NOT NULL DEFAULT '',
    memo              text NOT NULL DEFAULT '',
    -- UNIQUE: an entry can be reversed at most once.
    reverses_entry_id bigint UNIQUE REFERENCES journal_entries (id),
    xact_id           xid8 NOT NULL DEFAULT pg_current_xact_id(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (reverses_entry_id IS NULL OR reverses_entry_id < id)
);

CREATE TABLE postings (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entry_id   bigint NOT NULL REFERENCES journal_entries (id),
    account_id bigint NOT NULL,
    asset_code text NOT NULL,
    -- Signed: debit positive, credit negative.
    amount     minor_units NOT NULL CHECK (amount <> 0),
    CONSTRAINT postings_asset_matches_account
        FOREIGN KEY (account_id, asset_code) REFERENCES ledger_accounts (id, asset_code)
);

CREATE INDEX postings_entry_id_idx ON postings (entry_id);
CREATE INDEX postings_account_id_idx ON postings (account_id);

-- A cache of SUM(postings.amount) per account, maintained only by triggers.
-- The CHECK makes overdrafts impossible at the database level.
CREATE TABLE ledger_balances (
    account_id   bigint PRIMARY KEY,
    balance_rule text NOT NULL,
    balance      minor_units NOT NULL DEFAULT 0,
    FOREIGN KEY (account_id, balance_rule) REFERENCES ledger_accounts (id, balance_rule),
    CONSTRAINT ledger_balance_rule CHECK (
        balance_rule = 'any'
        OR (balance_rule = 'non_negative' AND balance >= 0)
        OR (balance_rule = 'non_positive' AND balance <= 0)
    )
);

-- +goose StatementBegin
CREATE FUNCTION ledger_reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger is append-only: % on % is not allowed', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation', HINT = 'post a reversing entry instead';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER journal_entries_append_only
    BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER journal_entries_no_truncate
    BEFORE TRUNCATE ON journal_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER postings_append_only
    BEFORE UPDATE OR DELETE ON postings
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER postings_no_truncate
    BEFORE TRUNCATE ON postings
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- +goose StatementBegin
CREATE FUNCTION ledger_balances_trigger_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Depth 1 means a statement issued directly against ledger_balances;
    -- the ledger's own triggers run at depth 2.
    IF pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'ledger_balances is maintained by triggers only'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ledger_balances rows are never deleted' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_balances_guard
    BEFORE INSERT OR UPDATE OR DELETE ON ledger_balances
    FOR EACH ROW EXECUTE FUNCTION ledger_balances_trigger_only();

-- +goose StatementBegin
CREATE FUNCTION ledger_open_balance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO ledger_balances (account_id, balance_rule) VALUES (NEW.id, NEW.balance_rule);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_accounts_open_balance
    AFTER INSERT ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_open_balance();

-- Accounts are reference data: their identity and rules never change.
-- +goose StatementBegin
CREATE FUNCTION ledger_accounts_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger accounts are immutable: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_accounts_no_change
    BEFORE UPDATE OR DELETE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_accounts_immutable();

-- +goose StatementBegin
CREATE FUNCTION ledger_posting_same_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    entry_xact xid8;
BEGIN
    SELECT xact_id INTO entry_xact FROM journal_entries WHERE id = NEW.entry_id;
    IF entry_xact IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'postings can only be added in the transaction that created entry %', NEW.entry_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER postings_same_transaction
    BEFORE INSERT ON postings
    FOR EACH ROW EXECUTE FUNCTION ledger_posting_same_transaction();

-- +goose StatementBegin
CREATE FUNCTION ledger_apply_posting() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Row lock on the balance; the ledger_balance_rule CHECK fires here.
    UPDATE ledger_balances SET balance = balance + NEW.amount WHERE account_id = NEW.account_id;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER postings_apply_to_balance
    AFTER INSERT ON postings
    FOR EACH ROW EXECUTE FUNCTION ledger_apply_posting();

-- +goose StatementBegin
CREATE FUNCTION ledger_check_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    bad record;
BEGIN
    SELECT asset_code, sum(amount) AS total INTO bad
    FROM postings WHERE entry_id = NEW.entry_id
    GROUP BY asset_code HAVING sum(amount) <> 0
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'journal entry % does not balance: % sums to %', NEW.entry_id, bad.asset_code, bad.total
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entry_balanced';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER postings_entry_balanced
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_entry_balanced();

-- +goose StatementBegin
CREATE FUNCTION ledger_check_entry_complete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    n int;
    original_reverses bigint;
BEGIN
    SELECT count(*) INTO n FROM postings WHERE entry_id = NEW.id;
    IF n < 2 THEN
        RAISE EXCEPTION 'journal entry % has % posting(s); at least 2 are required', NEW.id, n
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entry_min_postings';
    END IF;

    IF NEW.reverses_entry_id IS NOT NULL THEN
        SELECT reverses_entry_id INTO original_reverses
        FROM journal_entries WHERE id = NEW.reverses_entry_id;
        IF original_reverses IS NOT NULL THEN
            RAISE EXCEPTION 'entry % is itself a reversal and cannot be reversed', NEW.reverses_entry_id
                USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_no_reversal_of_reversal';
        END IF;
        -- The reversal's net effect per (account, asset) must be exactly the
        -- negation of the original's.
        IF EXISTS (
            (SELECT account_id, asset_code, sum(amount) FROM postings
              WHERE entry_id = NEW.id GROUP BY account_id, asset_code
             EXCEPT
             SELECT account_id, asset_code, -sum(amount) FROM postings
              WHERE entry_id = NEW.reverses_entry_id GROUP BY account_id, asset_code)
            UNION ALL
            (SELECT account_id, asset_code, -sum(amount) FROM postings
              WHERE entry_id = NEW.reverses_entry_id GROUP BY account_id, asset_code
             EXCEPT
             SELECT account_id, asset_code, sum(amount) FROM postings
              WHERE entry_id = NEW.id GROUP BY account_id, asset_code)
        ) THEN
            RAISE EXCEPTION 'entry % does not exactly reverse entry %', NEW.id, NEW.reverses_entry_id
                USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_reversal_mirrors_original';
        END IF;
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER journal_entries_complete
    AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_entry_complete();

-- +goose Down
DROP TABLE ledger_balances;
DROP TABLE postings;
DROP TABLE journal_entries;
DROP TABLE ledger_accounts;
DROP TABLE assets;
DROP FUNCTION ledger_check_entry_complete();
DROP FUNCTION ledger_check_entry_balanced();
DROP FUNCTION ledger_apply_posting();
DROP FUNCTION ledger_posting_same_transaction();
DROP FUNCTION ledger_accounts_immutable();
DROP FUNCTION ledger_open_balance();
DROP FUNCTION ledger_balances_trigger_only();
DROP FUNCTION ledger_reject_mutation();
DROP DOMAIN minor_units;
