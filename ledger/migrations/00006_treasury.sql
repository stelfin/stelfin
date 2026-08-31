-- +goose Up

-- How long a proposal may collect signatures.
--
-- settlement.DefaultTimeout is 180 seconds, which is right for one person
-- confirming a payment in a browser tab and fatally wrong for five people in
-- three time zones approving a payroll run. A proposal that expires before the
-- weekend is over is a proposal that will be rebuilt on Monday with a new
-- sequence number and re-signed from scratch by everyone who already agreed.
--
-- Capped at seven days rather than left open. An envelope's time bound is what
-- limits the damage of an approval someone regrets, and "no expiry" means a
-- signature collected today can be executed next year against a treasury whose
-- members have all changed.
ALTER TABLE orgs
    ADD COLUMN proposal_ttl interval NOT NULL DEFAULT '72 hours'
        CHECK (proposal_ttl >= interval '1 hour' AND proposal_ttl <= interval '7 days');


-- A treasury this org has proved control of.
--
-- A row here is not a claim, it is a proof: it exists only after a SEP-10
-- challenge was signed to the account's medium threshold, which is exactly the
-- weight needed to move its money. Anyone can type an address into a chat; the
-- point of this table is that typing is not enough.
--
-- Both custody models live in one table because the product must be able to say
-- which one a DAO is running. A classic M-of-N account and a Soroban smart
-- account are both "the treasury" to a member, and they differ in the one place
-- that matters: whether a policy contract can actually enforce anything.
CREATE TABLE org_treasuries (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,

    -- 'classic' is a G-account whose authority is signer weight; 'contract' is
    -- a C-address whose authority is __check_auth. The prefix and the kind are
    -- checked against each other, because a C-address routed down the classic
    -- path would have its weight counted — and would come out at zero, which
    -- reads like "nobody has signed yet" rather than "this question does not
    -- apply here".
    kind       text        NOT NULL CHECK (kind IN ('classic', 'contract')),
    address    text        NOT NULL CHECK (address ~ '^[GC][A-Z2-7]{55}$'),

    label      text        NOT NULL DEFAULT '',

    -- Thresholds as the account reported them when it was linked. Advisory: a
    -- signer removed five minutes ago is exactly the case an attacker wants,
    -- so every decision re-reads the account. These are here so the bot can
    -- say "2 of 3" without a network round trip on every keystroke.
    low_threshold    int   NOT NULL DEFAULT 0 CHECK (low_threshold BETWEEN 0 AND 255),
    medium_threshold int   NOT NULL DEFAULT 0 CHECK (medium_threshold BETWEEN 0 AND 255),
    high_threshold   int   NOT NULL DEFAULT 0 CHECK (high_threshold BETWEEN 0 AND 255),

    -- Set to the contract id for a smart account, so the signer router can tell
    -- the two apart without re-parsing the address.
    contract_id text,

    verified_at timestamptz NOT NULL DEFAULT now(),
    verified_by bigint      REFERENCES user_identities (id) ON DELETE SET NULL,
    refreshed_at timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT org_treasuries_kind_matches_address CHECK (
        (kind = 'classic'  AND address LIKE 'G%' AND contract_id IS NULL) OR
        (kind = 'contract' AND address LIKE 'C%' AND contract_id = address)
    )
);

-- One org per address. Two DAOs pointing at the same treasury would each post
-- the same on-chain payment to their own books, and the second copy would look
-- like money appearing from nowhere.
CREATE UNIQUE INDEX org_treasuries_address_key ON org_treasuries (address);

-- Needed by the composite foreign keys below, which is how tenant containment
-- stops being something every query has to remember.
CREATE UNIQUE INDEX org_treasuries_org_key ON org_treasuries (id, org_id);

CREATE INDEX org_treasuries_org_idx ON org_treasuries (org_id);


-- The account's signer set, as last read from the network.
--
-- A cache, and never an authority. Nothing may decide that an envelope is
-- sufficiently signed by reading these rows: the network decides, against the
-- account as it stands when the transaction is included, and the gap between
-- the two is precisely the window in which a removed signer still counts.
--
-- It exists so the bot can name who has not signed yet without asking Horizon
-- once per member per message.
CREATE TABLE treasury_signers (
    treasury_id bigint      NOT NULL REFERENCES org_treasuries (id) ON DELETE CASCADE,
    address     text        NOT NULL CHECK (address ~ '^[GC][A-Z2-7]{55}$'),
    weight      int         NOT NULL CHECK (weight BETWEEN 0 AND 255),
    refreshed_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (treasury_id, address)
);


-- An envelope waiting for enough signatures.
--
-- base_xdr is immutable and carries no signatures. Signatures are rows, and the
-- submittable envelope is rebuilt from those rows every time it is needed.
--
-- The tempting alternative — store one envelope and add each signature to it —
-- loses money on a Saturday afternoon. Two approvers signing within the same
-- second both read the envelope with one signature, both write it back with
-- two, and the second write erases the first. Nothing errors; the proposal just
-- never reaches its threshold and nobody can say why.
CREATE TABLE proposals (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    treasury_id bigint      NOT NULL,

    kind        text        NOT NULL CHECK (kind IN (
                                'payment', 'payroll', 'trustline',
                                'dex', 'signers', 'invoke')),

    -- The unsigned envelope, base64 XDR, exactly as every approver will see it.
    base_xdr    text        NOT NULL CHECK (base_xdr <> ''),

    -- The hash of that envelope: what each signature actually covers. Unique,
    -- because two proposals over one envelope would be two sets of signatures
    -- for one act, and executing either would consume the other.
    tx_hash     text        NOT NULL CHECK (tx_hash ~ '^[0-9a-f]{64}$'),

    -- The sequence number this envelope reserves.
    --
    -- Recorded because it is the silent killer of multisig: the envelope is
    -- built against source sequence N and is valid only at N+1, so anything
    -- else the treasury submits in the meantime invalidates it permanently —
    -- with no error, no event, and no signal until execution fails much later.
    -- Checked again at submit time so the failure is named rather than
    -- mysterious.
    source_seq  bigint      NOT NULL CHECK (source_seq >= 0),

    -- The canonical description every approver was shown, and its digest.
    --
    -- The digest is what the on-chain proposal carries as its memo, so the text
    -- a member read in chat and the proposal a contract voted on are provably
    -- the same document. Storing the text here rather than only the hash means
    -- the audit trail can still say what was approved a year later.
    description_canonical text  NOT NULL CHECK (description_canonical <> ''),
    description_digest    bytea NOT NULL CHECK (octet_length(description_digest) = 32),

    status      text        NOT NULL DEFAULT 'open' CHECK (status IN (
                                'open', 'executed', 'failed', 'cancelled', 'expired')),

    created_by  bigint      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    resolved_at timestamptz,

    -- The hash of what was actually submitted. Differs from tx_hash whenever
    -- the envelope was fee-bumped, which it usually is.
    submitted_hash text     CHECK (submitted_hash ~ '^[0-9a-f]{64}$'),

    FOREIGN KEY (treasury_id, org_id) REFERENCES org_treasuries (id, org_id) ON DELETE CASCADE,
    FOREIGN KEY (created_by, org_id) REFERENCES members (id, org_id),

    CONSTRAINT proposals_expires_after_creation CHECK (expires_at > created_at),
    CONSTRAINT proposals_resolved_iff_closed CHECK (
        (status = 'open') = (resolved_at IS NULL)
    )
);

CREATE UNIQUE INDEX proposals_tx_hash_key ON proposals (tx_hash);

-- One open proposal per treasury.
--
-- Not a queue discipline preference: two open proposals from one account both
-- reserve the same next sequence number, so whichever executes first silently
-- kills the other. Refusing the second at creation turns that into a message a
-- member can act on.
CREATE UNIQUE INDEX proposals_one_open_per_treasury
    ON proposals (treasury_id) WHERE status = 'open';

CREATE INDEX proposals_org_recent_idx ON proposals (org_id, created_at DESC);
CREATE INDEX proposals_expiry_idx ON proposals (expires_at) WHERE status = 'open';


-- +goose StatementBegin
-- The envelope under signature never changes.
--
-- Everything a signature covers — the envelope, its hash, the sequence it
-- reserves, the account it spends from — is frozen at creation. An UPDATE that
-- moved any of it would leave approvals attached to a document nobody approved,
-- which is the one way a correctly implemented multisig still loses money.
CREATE OR REPLACE FUNCTION proposals_reject_rewrite() RETURNS trigger AS $$
BEGIN
    IF NEW.base_xdr    IS DISTINCT FROM OLD.base_xdr
    OR NEW.tx_hash     IS DISTINCT FROM OLD.tx_hash
    OR NEW.source_seq  IS DISTINCT FROM OLD.source_seq
    OR NEW.treasury_id IS DISTINCT FROM OLD.treasury_id
    OR NEW.org_id      IS DISTINCT FROM OLD.org_id
    OR NEW.description_canonical IS DISTINCT FROM OLD.description_canonical
    OR NEW.description_digest    IS DISTINCT FROM OLD.description_digest
    THEN
        RAISE EXCEPTION
            'proposal % is under signature; its envelope and description cannot be rewritten',
            OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER proposals_envelope_immutable
    BEFORE UPDATE ON proposals
    FOR EACH ROW EXECUTE FUNCTION proposals_reject_rewrite();


-- One approval, as a row.
--
-- Rows rather than a mutated envelope, so two people approving at the same
-- moment produce two rows and not one lost signature.
CREATE TABLE proposal_signatures (
    proposal_id bigint      NOT NULL REFERENCES proposals (id) ON DELETE CASCADE,

    -- The signer as it appears on the account, not the member who pressed the
    -- button. A member may sign from a key stelfin has never heard of, and the
    -- network cares about the key.
    signer      text        NOT NULL CHECK (signer ~ '^G[A-Z2-7]{55}$'),

    -- The raw 64-byte Ed25519 signature over the envelope hash. The hint is
    -- derived from the signer rather than stored: storing it separately creates
    -- a second, forgeable claim about which key signed.
    signature   bytea       NOT NULL CHECK (octet_length(signature) = 64),

    -- The weight this signer carried when the signature was accepted. Written
    -- for the audit trail, never read to decide sufficiency.
    weight_at_signing int   NOT NULL CHECK (weight_at_signing BETWEEN 0 AND 255),

    added_by    bigint      REFERENCES user_identities (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    -- One signature per key per proposal. A second from the same key is not
    -- more approval, and counting it as such would report a threshold met that
    -- the network will refuse.
    PRIMARY KEY (proposal_id, signer)
);


-- What happened to a proposal, in order.
--
-- Append-only, and separate from the proposal's status column, because "it
-- failed" is not the same fact as "it was submitted, rejected for a bad
-- sequence, rebuilt, and submitted again". A treasury's members will eventually
-- want the second answer.
CREATE TABLE proposal_events (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    proposal_id bigint      NOT NULL REFERENCES proposals (id) ON DELETE CASCADE,
    kind        text        NOT NULL CHECK (kind IN (
                                'created', 'signed', 'submitted', 'executed',
                                'failed', 'cancelled', 'expired', 'superseded')),
    actor       bigint      REFERENCES user_identities (id) ON DELETE SET NULL,
    detail      text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX proposal_events_proposal_idx ON proposal_events (proposal_id, id);

CREATE TRIGGER proposal_events_immutable
    BEFORE UPDATE OR DELETE ON proposal_events
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

-- +goose Down

DROP TABLE proposal_events;
DROP TABLE proposal_signatures;
DROP TRIGGER proposals_envelope_immutable ON proposals;
DROP FUNCTION proposals_reject_rewrite();
DROP TABLE proposals;
DROP TABLE treasury_signers;
DROP TABLE org_treasuries;
ALTER TABLE orgs DROP COLUMN proposal_ttl;
