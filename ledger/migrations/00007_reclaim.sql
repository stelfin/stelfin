-- +goose Up

-- Transactions that hand a provisioned account back, awaiting its own
-- signature.
--
-- A rate limit without a way back is just a slow leak. Every account the
-- operator provisions locks 1 XLM of base reserve plus 0.5 per trustline, for
-- as long as the account exists — so a member who is finished with the wallet
-- stelfin paid for should be able to return the reserve, and this is the record
-- that makes the release safe to act on.
--
-- Mirrors pending_sends and pending_enrollments: the hash is what makes
-- submission safe, because it covers the transaction and not its signatures. A
-- matching hash proves the envelope is byte-for-byte the one that was shown, so
-- a signed submission cannot smuggle in a different destination for the balance
-- that is about to be swept.
--
-- That last point is why this table exists at all rather than the shape being
-- re-checked from the XDR at submit time. An AccountMerge sends the account's
-- entire XLM balance somewhere and deletes the account; "somewhere" is the one
-- field that must not be negotiable between building and signing.
CREATE TABLE pending_reclaims (
    hash        text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    owner_ref   text        NOT NULL,

    -- The account being handed back, and where its balance goes. Both stored,
    -- both checked against the envelope on the way back in.
    address     text        NOT NULL CHECK (address ~ '^G[A-Z2-7]{55}$'),
    destination text        NOT NULL CHECK (destination ~ '^G[A-Z2-7]{55}$'),

    -- The unsigned envelope, base64 XDR. What the page renders is re-derived
    -- from this rather than from stored display strings, the same rule the
    -- other two follow.
    envelope_xdr text       NOT NULL CHECK (envelope_xdr <> ''),

    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Mirrors the transaction's own time bounds, so a submission after this is
    -- refused here rather than by the network.
    expires_at   timestamptz NOT NULL,
    submitted_at timestamptz,

    CONSTRAINT pending_reclaims_expires_after_creation CHECK (expires_at > created_at),
    -- An account cannot be merged into itself; the network refuses it, and
    -- building one would mean the sponsor address was resolved wrongly.
    CONSTRAINT pending_reclaims_not_self CHECK (destination <> address)
);

-- One outstanding reclaim per account. Asking again supersedes the previous
-- attempt rather than accumulating envelopes that each sweep the same balance
-- to the same place — only one of which can ever land, with the rest failing
-- later for a reason nobody would connect to this.
CREATE UNIQUE INDEX pending_reclaims_live_key
    ON pending_reclaims (org_id, address) WHERE submitted_at IS NULL;

CREATE INDEX pending_reclaims_expiry_idx
    ON pending_reclaims (expires_at) WHERE submitted_at IS NULL;

-- +goose Down

DROP TABLE pending_reclaims;
