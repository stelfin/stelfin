-- +goose Up

-- Transactions this server built and showed to a user, awaiting their signature.
--
-- This table is what makes submission safe. The treasury pays the fee for every
-- transaction it fee-bumps, so a submit endpoint that accepted any signed
-- envelope would let anyone spend the treasury's XLM on transactions stelfin
-- never authored. Submission is therefore only accepted for a transaction hash
-- that appears here, issued to the submitting user.
--
-- The hash covers the transaction but not its signatures, so a matching hash
-- proves the envelope is byte-for-byte the one the user was shown — a signed
-- submission cannot smuggle in a different amount or destination.
CREATE TABLE pending_sends (
    -- Stellar transaction hash, lowercase hex.
    hash        text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    owner_ref   text        NOT NULL,

    -- What the user was shown, kept so the audit trail records the approval
    -- and not just the envelope.
    amount      bigint      NOT NULL CHECK (amount > 0),
    asset_id    smallint    NOT NULL REFERENCES assets (id),
    destination text        NOT NULL CHECK (destination ~ '^G[A-Z2-7]{55}$'),

    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Mirrors the transaction's own time bounds. A submission after this is
    -- rejected here rather than being fee-bumped and refused by the network.
    expires_at  timestamptz NOT NULL,
    -- Set once, when the envelope is accepted for submission. Prevents the
    -- treasury paying to fee-bump the same transaction repeatedly.
    submitted_at timestamptz,

    CONSTRAINT pending_sends_expires_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX pending_sends_owner_idx ON pending_sends (owner_ref, created_at DESC);
CREATE INDEX pending_sends_expiry_idx ON pending_sends (expires_at) WHERE submitted_at IS NULL;

-- +goose Down

DROP TABLE pending_sends;
