-- +goose Up

-- Saved recipients, keyed by the label a user actually says: "brother",
-- "mama", "landlord".
--
-- Resolution from label to address is stelfin's job and must be deterministic.
-- The language model may propose that a destination is a beneficiary label, but
-- it never decides which address that label means — otherwise a decode error
-- becomes a payment to the wrong person, which on Stellar is unrecoverable.
CREATE TABLE beneficiaries (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The owning user, matching ledger_accounts.owner_ref.
    owner_ref  text        NOT NULL,
    label      text        NOT NULL CHECK (label <> ''),
    address    text        NOT NULL CHECK (address ~ '^G[A-Z2-7]{55}$'),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One label per user, compared case-insensitively. Two saved recipients called
-- "brother" would make every "send to brother" a coin flip.
CREATE UNIQUE INDEX beneficiaries_owner_label_key
    ON beneficiaries (owner_ref, lower(label));

CREATE INDEX beneficiaries_owner_idx ON beneficiaries (owner_ref);

-- +goose Down

DROP TABLE beneficiaries;
