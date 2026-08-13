-- +goose Up

-- Transactions this server built to provision a new user's Stellar account,
-- awaiting that user's own signature.
--
-- Mirrors pending_sends: the hash is what makes submission safe (a matching
-- hash proves the envelope is the one issued, even though it was unsigned
-- when it was shown), and only a transaction that appears here, issued to the
-- submitting user, may be finalised into a stelfin account.
--
-- Keyed by hash rather than owner_ref because that is what a submission
-- authorises against — but a user may have only one *outstanding* enrollment
-- at a time, which the partial unique index below enforces. Requesting
-- enrollment again before completing it (a reloaded page, a regenerated
-- device key) supersedes the previous attempt rather than piling up orphaned
-- ones.
CREATE TABLE pending_enrollments (
    -- Stellar transaction hash, lowercase hex.
    hash        text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    owner_ref   text        NOT NULL,

    -- The address being provisioned. Not yet a Stellar account until this
    -- transaction lands — CreateAccount is what brings it into existence.
    address     text        NOT NULL CHECK (address ~ '^G[A-Z2-7]{55}$'),

    -- The unsigned envelope, base64 XDR. Re-derived on load rather than
    -- trusted from stored display strings, the same rule as pending_sends.
    envelope_xdr text       NOT NULL CHECK (envelope_xdr <> ''),

    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Mirrors the transaction's own time bounds, so a submission after this is
    -- rejected here rather than fee-... — there is no fee-bump on this
    -- transaction (the treasury is its source account directly), but the same
    -- reasoning applies: don't let the network be the first to say no.
    expires_at  timestamptz NOT NULL,
    -- Set once, when the envelope is accepted for submission.
    submitted_at timestamptz,

    CONSTRAINT pending_enrollments_expires_after_creation CHECK (expires_at > created_at)
);

-- One outstanding enrollment per user. A completed one (submitted_at set)
-- does not block a future re-enrollment attempt from ever being inserted,
-- but in practice PrepareEnrollment refuses to run one once the user has a
-- stellar_accounts row at all, so this is defence in depth rather than the
-- only thing preventing two live accounts for one phone number.
CREATE UNIQUE INDEX pending_enrollments_owner_pending_key
    ON pending_enrollments (owner_ref) WHERE submitted_at IS NULL;

CREATE INDEX pending_enrollments_expiry_idx
    ON pending_enrollments (expires_at) WHERE submitted_at IS NULL;

-- +goose Down

DROP TABLE pending_enrollments;
