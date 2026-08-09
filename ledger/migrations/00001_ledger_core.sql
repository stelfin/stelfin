-- +goose Up

-- The ledger is an append-only, double-entry index of on-chain state plus
-- internal bookkeeping. It is NOT authoritative for balances — Stellar is.
-- Its job is to make every movement explainable and to catch disagreement with
-- the chain during reconciliation.
--
-- Every invariant here is enforced by the database, not by application code.
-- Go can be bypassed by a migration, a psql session, or a future service; the
-- constraints cannot.

CREATE TABLE assets (
    id       smallint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code     text     NOT NULL CHECK (code <> ''),
    -- NULL issuer means the native asset (XLM). Every other asset is issued.
    issuer   text     CHECK (issuer IS NULL OR issuer ~ '^G[A-Z2-7]{55}$'),
    is_native boolean NOT NULL,

    CONSTRAINT assets_native_has_no_issuer
        CHECK ((is_native AND issuer IS NULL) OR (NOT is_native AND issuer IS NOT NULL))
);

-- A plain UNIQUE would treat two NULL issuers as distinct, which would let the
-- native asset be registered twice.
CREATE UNIQUE INDEX assets_code_issuer_key ON assets (code, COALESCE(issuer, ''));

-- Ledger accounts are internal bookkeeping accounts. They are not Stellar
-- accounts and must never be confused with them.
--
--   user              a stelfin user's position
--   treasury          XLM float for sponsorship and fee-bumps
--   external          the outside world; the counterparty for anything
--                     entering or leaving the system
--   fee_expense       fees paid out
--   sponsored_reserve CAP-33 reserves locked against user accounts. Tracked
--                     separately because a sponsored reserve is a liability
--                     that is reclaimable, not spendable float.
CREATE TABLE ledger_accounts (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind            text        NOT NULL CHECK (kind IN (
                        'user', 'treasury', 'external', 'fee_expense', 'sponsored_reserve')),
    -- Opaque owner reference: the user id for kind='user', NULL otherwise.
    owner_ref       text,
    name            text        NOT NULL CHECK (name <> ''),
    -- Only 'external' may hold a negative balance: it is the source and sink
    -- for value crossing the system boundary, so it mirrors everything held
    -- internally. A negative user or treasury balance means we have a bug.
    allows_negative boolean     NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_accounts_owner_ref_matches_kind
        CHECK ((kind = 'user') = (owner_ref IS NOT NULL)),
    CONSTRAINT ledger_accounts_only_external_goes_negative
        CHECK (allows_negative = (kind = 'external'))
);

CREATE UNIQUE INDEX ledger_accounts_user_key
    ON ledger_accounts (owner_ref) WHERE kind = 'user';

-- Singleton accounts: exactly one treasury, one external, one fee_expense, one
-- sponsored_reserve. Guards against a second treasury silently absorbing float.
CREATE UNIQUE INDEX ledger_accounts_singleton_key
    ON ledger_accounts (kind) WHERE kind <> 'user';

-- A journal entry: a set of lines that must balance.
CREATE TABLE ledger_transactions (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Caller-supplied and unique. This is what makes posting safe to retry: a
    -- replayed request collides here rather than double-posting.
    idempotency_key text        NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    -- SHA-256 over the canonical form of the posting request. A retry with the
    -- same key must carry the same content; if it does not, the caller has
    -- reused a key for different money and we fail loudly rather than returning
    -- a success that refers to someone else's transaction.
    request_fingerprint bytea   NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    kind            text        NOT NULL CHECK (kind IN (
                        'deposit', 'send', 'fee', 'sponsor', 'reserve_release', 'withdrawal')),
    -- Stellar transaction hash once known. Not unique: a single chain
    -- transaction can produce several ledger transactions (transfer + fee).
    external_ref    text        CHECK (external_ref IS NULL OR external_ref ~ '^[0-9a-f]{64}$'),
    -- When it happened on chain, versus when we recorded it. These diverge
    -- during ingestion catch-up and the difference matters for reconciliation.
    occurred_at     timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    metadata        jsonb       NOT NULL DEFAULT '{}'
);

CREATE INDEX ledger_transactions_external_ref_idx
    ON ledger_transactions (external_ref) WHERE external_ref IS NOT NULL;
CREATE INDEX ledger_transactions_occurred_at_idx ON ledger_transactions (occurred_at);

-- A single line of a journal entry. Amounts are signed stroops; a balanced
-- transaction sums to zero per asset.
CREATE TABLE ledger_entries (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id bigint      NOT NULL REFERENCES ledger_transactions (id),
    account_id     bigint      NOT NULL REFERENCES ledger_accounts (id),
    asset_id       smallint    NOT NULL REFERENCES assets (id),
    -- Zero-amount lines carry no information and hide mistakes. Reject them.
    amount         bigint      NOT NULL CHECK (amount <> 0),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ledger_entries_transaction_idx ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_account_asset_idx ON ledger_entries (account_id, asset_id);

-- Derived running balances, maintained by trigger. Reads are O(1) instead of
-- summing history. Correctness is checked by reconciling against SUM(entries)
-- in tests and by a periodic job in production — a balance table that silently
-- drifts from its entries is the classic ledger bug.
CREATE TABLE ledger_balances (
    account_id bigint      NOT NULL REFERENCES ledger_accounts (id),
    asset_id   smallint    NOT NULL REFERENCES assets (id),
    balance    bigint      NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (account_id, asset_id)
);


-- +goose StatementBegin
-- Zero-sum, enforced per (transaction, asset) rather than per transaction: a
-- send that pays its fee in XLM has two independent balanced groups, and
-- summing across assets would let a USDC imbalance be masked by an XLM one.
--
-- DEFERRABLE INITIALLY DEFERRED is essential. Entries are inserted one row at a
-- time, so the check can only be meaningful at COMMIT.
--
-- SUM(bigint) returns numeric in Postgres, so this cannot itself overflow.
CREATE OR REPLACE FUNCTION ledger_assert_balanced() RETURNS trigger AS $$
DECLARE
    offending record;
BEGIN
    SELECT e.asset_id, SUM(e.amount) AS total
      INTO offending
      FROM ledger_entries e
     WHERE e.transaction_id = NEW.transaction_id
     GROUP BY e.asset_id
    HAVING SUM(e.amount) <> 0
     LIMIT 1;

    IF FOUND THEN
        -- Custom SQLSTATE so Go can distinguish this from an ordinary CHECK
        -- constraint without matching on message text.
        RAISE EXCEPTION
            'ledger transaction % does not balance: asset % sums to %',
            NEW.transaction_id, offending.asset_id, offending.total
            USING ERRCODE = 'ST001';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_must_balance
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_balanced();


-- +goose StatementBegin
-- Append-only. Financial history is not editable: a correction is a new,
-- compensating transaction, never an UPDATE of the original.
CREATE OR REPLACE FUNCTION ledger_reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only; % is not permitted. Post a compensating transaction instead.',
        TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

CREATE TRIGGER ledger_transactions_immutable
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();


-- +goose StatementBegin
-- Maintain ledger_balances, and enforce the sign rule for the account kind.
-- A bigint overflow here raises rather than wrapping, which is what we want.
CREATE OR REPLACE FUNCTION ledger_apply_to_balance() RETURNS trigger AS $$
DECLARE
    new_balance     bigint;
    negative_is_ok  boolean;
BEGIN
    INSERT INTO ledger_balances AS b (account_id, asset_id, balance, updated_at)
    VALUES (NEW.account_id, NEW.asset_id, NEW.amount, now())
    ON CONFLICT (account_id, asset_id) DO UPDATE
        SET balance = b.balance + EXCLUDED.balance,
            updated_at = now()
    RETURNING b.balance INTO new_balance;

    SELECT a.allows_negative INTO negative_is_ok
      FROM ledger_accounts a
     WHERE a.id = NEW.account_id;

    IF new_balance < 0 AND NOT negative_is_ok THEN
        RAISE EXCEPTION
            'ledger account % would go negative (% stroops of asset %)',
            NEW.account_id, new_balance, NEW.asset_id
            USING ERRCODE = 'ST002';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_apply_balance
    AFTER INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_apply_to_balance();


-- The native asset is structural and its identity is not configurable.
-- Issued assets (USDC and friends) carry a network-specific issuer address and
-- are registered at startup from configuration, so that testnet and mainnet
-- issuers are never baked into a migration.
INSERT INTO assets (code, issuer, is_native) VALUES ('XLM', NULL, true);


-- +goose Down

DROP TABLE ledger_balances;
DROP TABLE ledger_entries;
DROP TABLE ledger_transactions;
DROP TABLE ledger_accounts;
DROP TABLE assets;
DROP FUNCTION IF EXISTS ledger_apply_to_balance();
DROP FUNCTION IF EXISTS ledger_reject_mutation();
DROP FUNCTION IF EXISTS ledger_assert_balanced();
