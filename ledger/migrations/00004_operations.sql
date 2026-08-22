-- +goose Up

-- Saved recipients, keyed by the label a member actually says: "payroll",
-- "the auditor", "ada".
--
-- Resolution from label to address is stelfin's job and must be deterministic.
-- The language model may propose that a destination is a beneficiary label, but
-- it never decides which address that label means — otherwise a decode error
-- becomes a payment to the wrong person, which on Stellar is unrecoverable.
--
-- This is also why writing the address book is a privileged, human-only act and
-- will never be exposed to an agent: whoever can add a label can decide where a
-- later "pay payroll" goes.
CREATE TABLE beneficiaries (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    -- The owning member, matching ledger_accounts.owner_ref.
    owner_ref  text        NOT NULL,
    label      text        NOT NULL CHECK (label <> ''),
    address    text        NOT NULL CHECK (address ~ '^G[A-Z2-7]{55}$'),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One label per owner per org, compared case-insensitively. Two saved
-- recipients called "payroll" would make every "send to payroll" a coin flip.
CREATE UNIQUE INDEX beneficiaries_owner_label_key
    ON beneficiaries (org_id, owner_ref, lower(label));

CREATE INDEX beneficiaries_owner_idx ON beneficiaries (org_id, owner_ref);


-- Transactions this server built and showed to someone, awaiting a signature.
--
-- This table is what makes submission safe. The operator pays the fee for every
-- transaction it fee-bumps, so a submit endpoint that accepted any signed
-- envelope would let anyone spend the operator's XLM on transactions stelfin
-- never authored. Submission is therefore only accepted for a transaction hash
-- that appears here, issued to the submitting owner.
--
-- The hash covers the transaction but not its signatures, so a matching hash
-- proves the envelope is byte-for-byte the one that was shown — a signed
-- submission cannot smuggle in a different amount or destination.
CREATE TABLE pending_sends (
    -- Stellar transaction hash, lowercase hex. Globally unique because a hash
    -- is: two orgs cannot produce the same envelope by accident.
    hash        text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    owner_ref   text        NOT NULL,

    -- The unsigned envelope, base64 XDR. The confirmation screen is rebuilt
    -- from this rather than from stored display strings: re-deriving what to
    -- show from the artifact under signature keeps the two from drifting, the
    -- same reason settlement.Describe reads a transaction back rather than
    -- trusting the request that built it.
    envelope_xdr text       NOT NULL CHECK (envelope_xdr <> ''),

    -- What the signer was shown, kept so the audit trail records the approval
    -- and not just the envelope.
    amount      bigint      NOT NULL CHECK (amount > 0),
    asset_id    smallint    NOT NULL REFERENCES assets (id),
    destination text        NOT NULL CHECK (destination ~ '^G[A-Z2-7]{55}$'),

    -- Display-only context: the recipient's saved label and the requester's own
    -- words. Never used to build or check a transaction.
    to_label         text   NOT NULL DEFAULT '',
    said_amount      text   NOT NULL DEFAULT '',
    said_destination text   NOT NULL DEFAULT '',

    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Mirrors the transaction's own time bounds. A submission after this is
    -- rejected here rather than being fee-bumped and refused by the network.
    expires_at  timestamptz NOT NULL,
    -- Set once, when the envelope is accepted for submission. Prevents the
    -- operator paying to fee-bump the same transaction repeatedly.
    submitted_at timestamptz,

    CONSTRAINT pending_sends_expires_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX pending_sends_owner_idx ON pending_sends (org_id, owner_ref, created_at DESC);
CREATE INDEX pending_sends_expiry_idx ON pending_sends (expires_at) WHERE submitted_at IS NULL;


-- Transactions this server built to provision a member's Stellar account,
-- awaiting that member's own signature.
--
-- Mirrors pending_sends: the hash is what makes submission safe, and only a
-- transaction that appears here, issued to the submitting owner, may be
-- finalised into a stelfin account.
--
-- Keyed by hash rather than owner_ref because that is what a submission
-- authorises against — but an owner may have only one *outstanding* enrollment
-- at a time, which the partial unique index below enforces. Requesting
-- enrollment again before completing it (a reloaded page, a regenerated device
-- key) supersedes the previous attempt rather than piling up orphans.
CREATE TABLE pending_enrollments (
    hash        text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    owner_ref   text        NOT NULL,

    -- The address being provisioned. Not yet a Stellar account until this
    -- transaction lands — CreateAccount is what brings it into existence.
    address     text        NOT NULL CHECK (address ~ '^G[A-Z2-7]{55}$'),

    -- The unsigned envelope, base64 XDR. Re-derived on load rather than
    -- trusted from stored display strings, the same rule as pending_sends.
    envelope_xdr text       NOT NULL CHECK (envelope_xdr <> ''),

    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Mirrors the transaction's own time bounds, so a submission after this is
    -- rejected here rather than letting the network be the first to say no.
    expires_at  timestamptz NOT NULL,
    submitted_at timestamptz,

    CONSTRAINT pending_enrollments_expires_after_creation CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX pending_enrollments_owner_pending_key
    ON pending_enrollments (org_id, owner_ref) WHERE submitted_at IS NULL;

CREATE INDEX pending_enrollments_expiry_idx
    ON pending_enrollments (expires_at) WHERE submitted_at IS NULL;


-- Every inbound chat message this server has already acted on.
--
-- A platform retries a delivery that is slow or fails, and the same id arrives
-- more than once. Claiming it exactly once is what stops one instruction
-- becoming two confirmations — and, if someone tapped both, two payments.
--
-- The id is the platform's own, prefixed by channel: a Telegram update id and
-- a Discord snowflake share no id space, and an unprefixed key would let one
-- hide the other.
--
-- Deliberately not org-scoped. The claim is made before an org is known in some
-- paths, and a delivery id is unique across the deployment anyway; scoping it
-- would only create a way for the same delivery to be processed twice.
CREATE TABLE processed_messages (
    id          text        PRIMARY KEY CHECK (id <> ''),
    sender      text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX processed_messages_received_idx ON processed_messages (received_at);

-- +goose Down

DROP TABLE processed_messages;
DROP TABLE pending_enrollments;
DROP TABLE pending_sends;
DROP TABLE beneficiaries;
