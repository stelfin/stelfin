-- +goose Up

-- Stellar addresses this deployment indexes, and the ledger account each one
-- mirrors.
--
-- Ingestion uses this to decide whether a payment on chain is something we
-- track at all, and which org's books it belongs in.
--
-- One address per ledger account and one ledger account per address. Not a
-- limitation carried over by accident: a payment arriving at an address has to
-- post to exactly one place, and any other mapping makes ingestion ambiguous
-- in a way that shows up as a wrong balance rather than an error.
--
-- A member is not the *owner* of a DAO treasury address under this model —
-- they are a signer on it, which is a different table entirely, because the
-- treasury's balance is the org's and not theirs.
CREATE TABLE tracked_addresses (
    address           text        PRIMARY KEY CHECK (address ~ '^G[A-Z2-7]{55}$'),
    org_id            bigint      NOT NULL REFERENCES orgs (id),
    ledger_account_id bigint      NOT NULL,
    role              text        NOT NULL CHECK (role IN ('treasury', 'member', 'sponsor')),
    created_at        timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (ledger_account_id, org_id)
        REFERENCES ledger_accounts (id, org_id)
);

CREATE UNIQUE INDEX tracked_addresses_account_key ON tracked_addresses (ledger_account_id);
CREATE INDEX tracked_addresses_org_idx ON tracked_addresses (org_id);


-- Resumable position in an ingestion stream.
--
-- Deliberately not org-scoped. A Horizon or Soroban cursor is a position in the
-- network's history, which every tenant shares; one cursor per org would mean
-- N passes over the same ledgers and N ways to fall behind.
--
-- Ingestion is at-least-once by design: the cursor advances only after the
-- corresponding ledger transaction is committed, so a crash in between replays
-- the operation rather than losing it. Replays are harmless because each
-- operation posts under an idempotency key derived from its own on-chain id.
--
-- The reverse order — advancing the cursor first — would be at-most-once and
-- would silently drop payments, which is the one failure a payments system
-- cannot tolerate.
CREATE TABLE ingestion_cursors (
    stream     text        PRIMARY KEY CHECK (stream <> ''),
    cursor     text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE ingestion_cursors;
DROP TABLE tracked_addresses;
