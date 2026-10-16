-- Transaction manager: one writer per signing key, and the nonce sequence
-- it hands out (invariant I10). See docs/design/txmgr.md.

-- +goose Up

CREATE TABLE signer_nonces (
    signer           text NOT NULL CHECK (signer ~ '^0x[0-9a-f]{40}$'),
    network          text NOT NULL,
    -- The lowest nonce not yet handed out. Never decreases.
    next_nonce       bigint NOT NULL DEFAULT 0 CHECK (next_nonce >= 0),
    -- The one process allowed to assign nonces for this key until the
    -- lease expires.
    lease_owner      text NOT NULL CHECK (length(lease_owner) BETWEEN 1 AND 200),
    lease_expires_at timestamptz NOT NULL,
    PRIMARY KEY (signer, network)
);

-- next_nonce only ever grows: handing out a nonce twice is how two
-- transactions end up competing for one slot.
-- +goose StatementBegin
CREATE FUNCTION signer_nonces_never_decrease() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.next_nonce < OLD.next_nonce THEN
        RAISE EXCEPTION 'next_nonce for % on % cannot decrease (% -> %)',
            OLD.signer, OLD.network, OLD.next_nonce, NEW.next_nonce
            USING ERRCODE = 'check_violation', CONSTRAINT = 'signer_nonces_never_decrease';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER signer_nonces_never_decrease BEFORE UPDATE ON signer_nonces
    FOR EACH ROW EXECUTE FUNCTION signer_nonces_never_decrease();

-- +goose Down
DROP TRIGGER signer_nonces_never_decrease ON signer_nonces;
DROP FUNCTION signer_nonces_never_decrease();
DROP TABLE signer_nonces;
