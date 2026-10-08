-- +goose Up

-- A code that attaches a second chat space to a workspace.
--
-- A DAO with a Discord server and a Telegram group is one tenant, with one
-- treasury and one set of members. The first space is registered by /setup;
-- every space after that has to be attached by someone who administers the
-- workspace already, or anybody who could add the bot to a group could attach
-- that group to somebody else's treasury and read its books.
--
-- So the authority flows one way: an admin of the workspace asks for a code,
-- and an administrator of the new space presents it there. Each side proves the
-- half it can — standing in the workspace, and standing in the space.
--
-- Only a hash is stored. The code is a bearer credential for as long as it
-- lives, and a database dump should be a list of what was issued rather than a
-- set of working keys.
CREATE TABLE space_link_codes (
    code_hash   text        PRIMARY KEY CHECK (code_hash ~ '^[0-9a-f]{64}$'),
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    issued_by   bigint      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,

    FOREIGN KEY (issued_by, org_id) REFERENCES members (id, org_id) ON DELETE CASCADE,
    CONSTRAINT space_link_codes_expires_after_creation CHECK (expires_at > created_at)
);

-- One live code per workspace: asking again replaces the last one, so a code
-- pasted somewhere it should not have been can be killed by asking for another.
CREATE UNIQUE INDEX space_link_codes_live_key
    ON space_link_codes (org_id) WHERE consumed_at IS NULL;


-- Spend ceilings stelfin enforces itself.
--
-- A classic M-of-N treasury has no contract to enforce anything: whatever its
-- signers sign, the network executes. These are a second limit, checked before
-- an envelope is ever built, so that a proposal larger than the workspace said
-- it wanted to see is never put in front of signers at all — the failure being
-- prevented is not theft, it is four tired people approving an extra zero.
--
-- Zero means no ceiling of this kind. That is the default, and it is safe as a
-- default only because the deployment has ceilings of its own above these.
ALTER TABLE orgs
    ADD COLUMN payment_ceiling_stroops bigint NOT NULL DEFAULT 0
        CHECK (payment_ceiling_stroops >= 0),
    ADD COLUMN daily_ceiling_stroops bigint NOT NULL DEFAULT 0
        CHECK (daily_ceiling_stroops >= 0);

-- What a proposal moves out of its treasury, in the deployment's asset.
--
-- Recorded when the proposal is created so that "how much has this workspace
-- proposed today" is a sum over a column and not a re-parse of every envelope.
-- Null for a proposal that moves nothing out — a signer change, a trustline.
ALTER TABLE proposals
    ADD COLUMN amount_stroops bigint CHECK (amount_stroops IS NULL OR amount_stroops > 0);

-- +goose Down

ALTER TABLE proposals DROP COLUMN amount_stroops;
ALTER TABLE orgs
    DROP COLUMN daily_ceiling_stroops,
    DROP COLUMN payment_ceiling_stroops;
DROP TABLE space_link_codes;
