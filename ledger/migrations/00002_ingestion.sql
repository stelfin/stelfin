-- +goose Up

-- Maps a Stellar account to the ledger account that mirrors it. Ingestion uses
-- this to decide whether a payment on chain is something we track at all.
--
-- One Stellar address per ledger account and vice versa: a user's position is
-- exactly one on-chain account, and conflating two would make every balance
-- ambiguous.
CREATE TABLE stellar_accounts (
    address           text        PRIMARY KEY CHECK (address ~ '^G[A-Z2-7]{55}$'),
    ledger_account_id bigint      NOT NULL REFERENCES ledger_accounts (id),
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX stellar_accounts_ledger_account_key
    ON stellar_accounts (ledger_account_id);

-- Resumable position in a Horizon stream.
--
-- Ingestion is at-least-once by design: the cursor advances only after the
-- corresponding ledger transaction is committed, so a crash in between replays
-- the operation rather than losing it. Replays are harmless because each
-- operation posts under an idempotency key derived from its Horizon id.
--
-- The reverse order — advancing the cursor first — would be at-most-once and
-- would silently drop payments, which is the one failure mode a payments
-- system cannot tolerate.
CREATE TABLE ingestion_cursors (
    stream     text        PRIMARY KEY CHECK (stream <> ''),
    cursor     text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE ingestion_cursors;
DROP TABLE stellar_accounts;
