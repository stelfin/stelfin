-- +goose Up

-- A third thing a signed challenge can prove: that a treasury's signers have
-- authorised a connector.
ALTER TABLE auth_challenges DROP CONSTRAINT auth_challenges_purpose_check;
ALTER TABLE auth_challenges ADD CONSTRAINT auth_challenges_purpose_check
    CHECK (purpose IN ('link_member', 'link_treasury', 'grant_connector'));

-- The tools a workspace has registered: a spreadsheet, somebody's MCP server,
-- an HTTP endpoint that returns rows.
--
-- Registering one grants it nothing. A row here says where a connector is and
-- how to reach it; whether stelfin will ever call it is decided by a grant,
-- below, and a grant takes signatures.
CREATE TABLE connectors (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,

    -- What the workspace calls it, and what a grant is recorded against. A
    -- name and not a URL, because the name is typed into commands and shown to
    -- signers, and a URL may be a secret.
    name        text        NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,30}$'),
    kind        text        NOT NULL CHECK (kind IN ('sheets', 'mcp', 'webhook')),

    -- Where it is. Empty for sheets, which has one endpoint and it is Google's.
    --
    -- Sealed, like the credential, and for the same reason: a webhook URL very
    -- often carries its own secret in the path, and a database dump should not
    -- be a list of working ones. Sealed against (org, name), so a row copied
    -- into another workspace decrypts to nothing.
    sealed_endpoint   text  NOT NULL DEFAULT '',
    -- The host alone, in the clear, because it is what a signer is shown and
    -- what a listing prints. Never the path.
    endpoint_host     text  NOT NULL DEFAULT '',
    -- sha256 of the full endpoint, hex. What a grant commits to, so the endpoint
    -- behind a granted name cannot be swapped without the grant ceasing to
    -- match — without the full URL ever being shown or stored in the clear.
    endpoint_digest   text  NOT NULL DEFAULT ''
                            CHECK (endpoint_digest = '' OR endpoint_digest ~ '^[0-9a-f]{64}$'),
    sealed_credential text  NOT NULL DEFAULT '',

    -- For an MCP server: the tools it offered when it was set up, and the
    -- digest over their names, descriptions and schemas. Every call re-lists
    -- the server's tools and refuses if this no longer matches.
    tools       text[]      NOT NULL DEFAULT '{}',
    tool_digest text        NOT NULL DEFAULT ''
                            CHECK (tool_digest = '' OR tool_digest ~ '^[0-9a-f]{64}$'),

    -- Set once whatever the kind needs has been supplied and checked. A
    -- connector that is not ready cannot be granted, because there is nothing
    -- definite yet for a grant to pin.
    ready_at    timestamptz,

    created_by  bigint      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    removed_at  timestamptz,

    UNIQUE (id, org_id),
    FOREIGN KEY (created_by, org_id) REFERENCES members (id, org_id)
);

-- One live connector per name per workspace. Removed ones keep their rows: a
-- grant that once existed should still be able to say what it was for.
CREATE UNIQUE INDEX connectors_live_name_key
    ON connectors (org_id, name) WHERE removed_at IS NULL;


-- What a treasury's signers have authorised a connector to do.
--
-- The authority is the signatures, and they are kept: signed_xdr is the
-- challenge the signers signed, which commits to statement_digest, which is the
-- hash of statement. Anyone holding this row can check all three against the
-- treasury's signer set without asking this deployment to vouch for any of it.
--
-- That is what stands in for the on-chain registry. Nothing here is taken on a
-- chat role's word: a row with no signatures is a request, and the guard reads
-- only rows that have them.
CREATE TABLE connector_grants (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id        bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    connector_id  bigint      NOT NULL,
    treasury_id   bigint      NOT NULL,

    capabilities   text[]     NOT NULL CHECK (cardinality(capabilities) > 0),
    surface_digest text       NOT NULL CHECK (surface_digest ~ '^[0-9a-f]{64}$'),
    per_hour       int        NOT NULL CHECK (per_hour BETWEEN 1 AND 3600),

    -- The text the signers were shown, exactly, and its hash.
    statement        text     NOT NULL CHECK (statement <> ''),
    statement_digest text     NOT NULL CHECK (statement_digest ~ '^[0-9a-f]{64}$'),

    -- The challenge this request is waiting on, or was satisfied by.
    challenge_hash text       NOT NULL CHECK (challenge_hash ~ '^[0-9a-f]{64}$'),
    signed_xdr     text       NOT NULL DEFAULT '',
    signed_by      text[]     NOT NULL DEFAULT '{}',

    requested_by  bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    granted_at    timestamptz,
    revoked_at    timestamptz,
    revoked_by    bigint,

    FOREIGN KEY (connector_id, org_id) REFERENCES connectors (id, org_id) ON DELETE CASCADE,
    FOREIGN KEY (requested_by, org_id) REFERENCES members (id, org_id),

    -- Granted means signed, and the row says by whom.
    CONSTRAINT connector_grants_granted_means_signed
        CHECK (granted_at IS NULL OR (signed_xdr <> '' AND cardinality(signed_by) > 0))
);

CREATE UNIQUE INDEX connector_grants_challenge_key ON connector_grants (challenge_hash);

-- At most one grant in force per connector. Granting again replaces the last
-- one in the same transaction, so there is never a moment with two.
CREATE UNIQUE INDEX connector_grants_live_key
    ON connector_grants (connector_id) WHERE granted_at IS NOT NULL AND revoked_at IS NULL;


-- A payroll read from a connector, kept so it can be looked at before anyone
-- commits to it.
--
-- Reading a sheet and proposing its contents used to be one step, which meant a
-- sheet with a bad row could only be discovered by trying to pay it, and a
-- draft could not be handed from the person who keeps the sheet to the person
-- who proposes. A draft is the rows as they were read, the reading of each one,
-- and nothing else: it moves no money and reserves nothing.
CREATE TABLE drafts (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id        bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    connector_id  bigint      NOT NULL,
    selector      text        NOT NULL DEFAULT '',
    memo          text        NOT NULL DEFAULT '',

    -- Each row: line, the text that was read, and what it was read as. The
    -- destination is stored resolved, to an address, so that handing the draft
    -- to somebody else cannot change where a row goes — a saved name means a
    -- different address to a different member.
    rows          jsonb       NOT NULL,
    row_count     int         NOT NULL CHECK (row_count > 0),
    total_stroops bigint      NOT NULL CHECK (total_stroops > 0),

    status        text        NOT NULL DEFAULT 'open'
                              CHECK (status IN ('open', 'proposed', 'discarded')),
    proposal_id   bigint      REFERENCES proposals (id),

    created_by    bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at   timestamptz,

    FOREIGN KEY (connector_id, org_id) REFERENCES connectors (id, org_id) ON DELETE CASCADE,
    FOREIGN KEY (created_by, org_id) REFERENCES members (id, org_id),
    CONSTRAINT drafts_proposed_names_a_proposal
        CHECK ((status = 'proposed') = (proposal_id IS NOT NULL))
);

CREATE INDEX drafts_org_recent_idx ON drafts (org_id, created_at DESC);

-- +goose Down

DROP TABLE drafts;
DROP TABLE connector_grants;
DROP TABLE connectors;
DELETE FROM auth_challenges WHERE purpose = 'grant_connector';
ALTER TABLE auth_challenges DROP CONSTRAINT auth_challenges_purpose_check;
ALTER TABLE auth_challenges ADD CONSTRAINT auth_challenges_purpose_check
    CHECK (purpose IN ('link_member', 'link_treasury'));
