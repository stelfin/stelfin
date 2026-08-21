-- +goose Up

-- The ledger is an append-only, double-entry index of on-chain state plus
-- internal bookkeeping. It is NOT authoritative for balances — Stellar is.
-- Its job is to make every movement explainable and to catch disagreement with
-- the chain during reconciliation.
--
-- Every invariant here is enforced by the database, not by application code.
-- Go can be bypassed by a migration, a psql session, or a future service; the
-- constraints cannot.
--
-- This deployment serves many DAOs, so a second class of invariant appears
-- alongside the accounting ones: nothing may reach across tenants. That is
-- enforced the same way, by constraints and a trigger, rather than by
-- remembering to write a predicate in every query.


-- An organisation. One per DAO, plus exactly one for the deployment itself.
--
-- The platform org exists so the operator's own money — the XLM float it
-- sponsors reserves and pays fees from — has somewhere to live that is not a
-- special case. The alternative, a nullable org_id meaning "ours", puts a
-- branch in every query and a NULL in every join.
CREATE TABLE orgs (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind         text        NOT NULL CHECK (kind IN ('platform', 'dao')),
    slug         text        NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,38}$'),
    display_name text        NOT NULL CHECK (display_name <> ''),

    -- An org is bound to one network for its lifetime. A treasury address is
    -- only meaningful on the network it exists on, and a mixed-network org
    -- would let an approval signed against testnet be presented for a mainnet
    -- envelope.
    network      text        NOT NULL CHECK (network IN ('testnet', 'public')),

    -- 'suspended' refuses every money-moving command for this tenant while
    -- leaving its history readable.
    status       text        NOT NULL DEFAULT 'active'
                             CHECK (status IN ('active', 'suspended')),

    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX orgs_slug_key ON orgs (lower(slug));

-- Exactly one platform org. A second would quietly split the operator's float
-- in two, and the shortfall would look like a reconciliation error rather than
-- a configuration one.
CREATE UNIQUE INDEX orgs_platform_singleton ON orgs (kind) WHERE kind = 'platform';


-- Where a bot has been installed, and which org that installation belongs to.
--
-- (channel, space_id) is the primary key, so a Telegram group or a Discord
-- guild maps to exactly one org and the lookup is total. This is the first
-- thing resolved for an inbound message, before any of its content is
-- interpreted: an unregistered space is not an error, it is silence.
--
-- Several spaces per org is normal and expected. A DAO with both a Discord
-- guild and a Telegram supergroup is one org, one member roster, one treasury.
CREATE TABLE org_spaces (
    org_id       bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    channel      text        NOT NULL CHECK (channel IN ('telegram', 'discord')),
    -- The platform's own space identifier, as text: a Telegram chat id
    -- (negative for supergroups) or a Discord guild id.
    space_id     text        NOT NULL CHECK (space_id <> ''),
    installed_by text        NOT NULL DEFAULT '',
    active       boolean     NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (channel, space_id)
);

CREATE INDEX org_spaces_org_idx ON org_spaces (org_id) WHERE active;


-- Assets are global, not per-org.
--
-- Two DAOs holding USDC must share one asset row, or ingestion would have to
-- decide which org's USDC an incoming payment is denominated in — a question
-- with no answer, since the asset on chain is the same asset. Which assets a
-- given org transacts in is a separate fact, recorded in org_assets.
CREATE TABLE assets (
    id        smallint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code      text     NOT NULL CHECK (code <> ''),
    -- NULL issuer means the native asset (XLM). Every other asset is issued.
    issuer    text     CHECK (issuer IS NULL OR issuer ~ '^G[A-Z2-7]{55}$'),
    is_native boolean  NOT NULL,

    CONSTRAINT assets_native_has_no_issuer
        CHECK ((is_native AND issuer IS NULL) OR (NOT is_native AND issuer IS NOT NULL))
);

-- A plain UNIQUE would treat two NULL issuers as distinct, which would let the
-- native asset be registered twice.
CREATE UNIQUE INDEX assets_code_issuer_key ON assets (code, COALESCE(issuer, ''));


-- Which assets an org transacts in.
CREATE TABLE org_assets (
    org_id     bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    asset_id   smallint    NOT NULL REFERENCES assets (id),
    is_default boolean     NOT NULL DEFAULT false,
    added_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (org_id, asset_id)
);

CREATE UNIQUE INDEX org_assets_default_key ON org_assets (org_id) WHERE is_default;


-- Ledger accounts are internal bookkeeping accounts. They are not Stellar
-- accounts and must never be confused with them.
--
--   member            a member's position within one org
--   treasury          an org's holdings. Unlike the others, not a singleton:
--                     a DAO may legitimately run an ops wallet and a grants
--                     wallet, and which is primary is a policy question rather
--                     than a uniqueness one
--   external          the outside world; the counterparty for anything
--                     entering or leaving this org's books
--   fee_expense       fees paid out
--   sponsored_reserve CAP-33 reserves locked against provisioned accounts.
--                     Tracked separately because a sponsored reserve is a
--                     reclaimable liability, not spendable float
--   trading           the counterparty for a swap. A trade is two balanced
--                     legs in different assets, and without a counterparty
--                     account it cannot be recorded at all: the per-asset
--                     zero-sum rule below would reject it, correctly
CREATE TABLE ledger_accounts (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id          bigint      NOT NULL REFERENCES orgs (id),
    kind            text        NOT NULL CHECK (kind IN (
                        'member', 'treasury', 'external', 'fee_expense',
                        'sponsored_reserve', 'trading')),
    -- Opaque owner reference: the member's owner reference for kind='member',
    -- NULL otherwise. No format is imposed — it is whatever the identity layer
    -- produces, currently "<channel>:<platform user id>".
    owner_ref       text,
    name            text        NOT NULL CHECK (name <> ''),
    -- Only counterparty accounts may hold a negative balance: they mirror
    -- everything held internally, so they are negative by construction. A
    -- negative member or treasury balance means we have a bug.
    allows_negative boolean     NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_accounts_owner_ref_matches_kind
        CHECK ((kind = 'member') = (owner_ref IS NOT NULL)),
    CONSTRAINT ledger_accounts_counterparties_go_negative
        CHECK (allows_negative = (kind IN ('external', 'trading'))),

    -- Redundant given the primary key, and load-bearing anyway: it lets child
    -- tables carry a composite foreign key on (account_id, org_id), which
    -- makes "this row belongs to the same org as its account" a database fact
    -- rather than a join someone has to remember to write.
    CONSTRAINT ledger_accounts_id_org_key UNIQUE (id, org_id)
);

CREATE UNIQUE INDEX ledger_accounts_member_key
    ON ledger_accounts (org_id, owner_ref) WHERE kind = 'member';

-- Singleton accounts, now per org rather than per database: one external, one
-- fee_expense, one sponsored_reserve, one trading, for each org. Guards against
-- a second one silently absorbing float. Treasury is deliberately exempt.
CREATE UNIQUE INDEX ledger_accounts_org_singleton_key
    ON ledger_accounts (org_id, kind)
    WHERE kind IN ('external', 'fee_expense', 'sponsored_reserve', 'trading');

CREATE UNIQUE INDEX ledger_accounts_treasury_name_key
    ON ledger_accounts (org_id, lower(name)) WHERE kind = 'treasury';


-- A journal entry: a set of lines that must balance.
CREATE TABLE ledger_transactions (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id          bigint      NOT NULL REFERENCES orgs (id),
    -- Caller-supplied. This is what makes posting safe to retry: a replayed
    -- request collides here rather than double-posting.
    --
    -- Unique per org, not globally. Two DAOs ingesting the same Horizon
    -- operation — a payment between their treasuries — each record it in their
    -- own books, and a global unique would let the first to arrive silence the
    -- second.
    idempotency_key text        NOT NULL CHECK (idempotency_key <> ''),
    -- SHA-256 over the canonical form of the posting request. A retry with the
    -- same key must carry the same content; if it does not, the caller has
    -- reused a key for different money and we fail loudly rather than returning
    -- a success that refers to someone else's transaction.
    request_fingerprint bytea   NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    kind            text        NOT NULL CHECK (kind IN (
                        'deposit', 'send', 'fee', 'sponsor', 'reserve_release',
                        'withdrawal', 'trade')),
    -- Stellar transaction hash once known. Not unique: a single chain
    -- transaction can produce several ledger transactions (transfer + fee).
    external_ref    text        CHECK (external_ref IS NULL OR external_ref ~ '^[0-9a-f]{64}$'),
    -- When it happened on chain, versus when we recorded it. These diverge
    -- during ingestion catch-up and the difference matters for reconciliation.
    occurred_at     timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    metadata        jsonb       NOT NULL DEFAULT '{}',

    CONSTRAINT ledger_transactions_org_key_key UNIQUE (org_id, idempotency_key)
);

CREATE INDEX ledger_transactions_external_ref_idx
    ON ledger_transactions (external_ref) WHERE external_ref IS NOT NULL;

-- Ordered for keyset pagination: history is read newest-first per org, and a
-- DAO's history grows without bound, so paging by OFFSET would scan everything
-- before the page being asked for.
CREATE INDEX ledger_transactions_org_occurred_idx
    ON ledger_transactions (org_id, occurred_at DESC, id DESC);


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
-- No transaction may span two orgs.
--
-- This is the tenancy invariant, and it is here rather than in query-writing
-- discipline for the same reason the accounting ones are: a predicate can be
-- forgotten in one query out of forty, and the symptom would be one DAO's money
-- moving into another DAO's books.
--
-- Value legitimately crossing between tenants leaves one org through its
-- external account and enters the other through theirs, as two transactions.
-- There is no case where a single balanced entry set should touch both.
CREATE OR REPLACE FUNCTION ledger_assert_single_org() RETURNS trigger AS $$
DECLARE
    offending record;
BEGIN
    SELECT t.org_id AS tx_org, a.org_id AS entry_org
      INTO offending
      FROM ledger_entries e
      JOIN ledger_accounts a ON a.id = e.account_id
      JOIN ledger_transactions t ON t.id = e.transaction_id
     WHERE e.transaction_id = NEW.transaction_id
       AND a.org_id <> t.org_id
     LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION
            'ledger transaction % belongs to org % but touches an account in org %',
            NEW.transaction_id, offending.tx_org, offending.entry_org
            USING ERRCODE = 'ST003';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_single_org
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_single_org();


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
DROP TABLE org_assets;
DROP TABLE assets;
DROP TABLE org_spaces;
DROP TABLE orgs;
DROP FUNCTION IF EXISTS ledger_apply_to_balance();
DROP FUNCTION IF EXISTS ledger_reject_mutation();
DROP FUNCTION IF EXISTS ledger_assert_single_org();
DROP FUNCTION IF EXISTS ledger_assert_balanced();
