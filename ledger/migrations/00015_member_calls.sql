-- +goose Up

-- A contract call one member makes, waiting for that member to sign it.
--
-- A treasury held by a contract is not signed for the way a classic account
-- is. Nobody collects signatures on one envelope: each member makes their own
-- call — proposing, voting, withdrawing — from their own address, and the
-- contract counts the votes. So what has to be remembered between the chat
-- message and the browser is one transaction, for one person, and whether it
-- has been sent.
--
-- The row is what makes submission safe to offer. The operator pays the fee
-- for every one of these, and an endpoint that sent whatever it was handed
-- would spend that on transactions stelfin never built. A call is accepted
-- only if it hashes to a row here, issued to whoever is submitting it — and a
-- transaction hash covers the transaction and not its signatures, so the
-- match proves it is the call that was shown while still allowing the
-- signature that was missing when it was.
CREATE TABLE member_calls (
    hash         text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    org_id       bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    owner_ref    text        NOT NULL CHECK (owner_ref <> ''),
    member_id    bigint      NOT NULL,
    treasury_id  bigint      NOT NULL,

    -- Who signs it and what it calls. Both are in the envelope too, and the
    -- envelope is what counts; these are here to be searched by.
    source       text        NOT NULL CHECK (source ~ '^G[A-Z2-7]{55}$'),
    contract     text        NOT NULL CHECK (contract ~ '^C[A-Z2-7]{55}$'),
    function     text        NOT NULL CHECK (function ~ '^[a-zA-Z_][a-zA-Z0-9_]{0,31}$'),
    envelope_xdr text        NOT NULL CHECK (envelope_xdr <> ''),

    -- What stelfin said this call was, in the chat that asked for it. Kept so
    -- the page can show it beside what the envelope says, marked as the part
    -- the page has not checked.
    says         text        NOT NULL DEFAULT '',

    -- A proposal's note, in the proposer's words, and the commitment to it
    -- that goes on chain. The contract stores the hash and not the prose: a
    -- ledger is a bad place for one and a good place for the other.
    note         text        NOT NULL DEFAULT '',
    memo         bytea       CHECK (memo IS NULL OR octet_length(memo) = 32),

    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Claimed when it is submitted, which is what stops two submissions of one
    -- envelope both being paid for.
    submitted_at timestamptz,

    -- What became of it. landed_hash is the fee bump's, which is what an
    -- explorer finds; failed means it reached a ledger and the contract
    -- refused, which spends the member's sequence number and nothing else.
    landed_hash   text       CHECK (landed_hash ~ '^[0-9a-f]{64}$'),
    landed_ledger int,
    failed        boolean    NOT NULL DEFAULT false,

    -- For a call that opened a proposal: the number the contract gave it.
    chain_proposal int       CHECK (chain_proposal IS NULL OR chain_proposal > 0),

    CONSTRAINT member_calls_expires_after_creation CHECK (expires_at > created_at),
    FOREIGN KEY (member_id, org_id) REFERENCES members (id, org_id),
    FOREIGN KEY (treasury_id, org_id) REFERENCES org_treasuries (id, org_id) ON DELETE CASCADE
);

CREATE INDEX member_calls_owner_idx ON member_calls (org_id, owner_ref, created_at DESC);

-- The note behind a proposal is found by the number the contract gave it.
CREATE UNIQUE INDEX member_calls_proposal_key
    ON member_calls (treasury_id, chain_proposal) WHERE chain_proposal IS NOT NULL;

-- +goose Down

DROP TABLE member_calls;
