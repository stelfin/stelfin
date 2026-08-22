-- +goose Up

-- A member of one org.
--
-- A member exists the moment someone speaks in a registered space, and has no
-- authority over money until an address is bound to them by signature. The two
-- facts are deliberately separate: the bot needs somewhere to hang a display
-- name and a role long before anyone proves they control a wallet.
CREATE TABLE members (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id         bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    display_name   text        NOT NULL DEFAULT '',

    -- The Stellar account this member signs with, once proved.
    address        text        CHECK (address IS NULL OR address ~ '^G[A-Z2-7]{55}$'),
    -- How they came by it: they connected one they already controlled, or the
    -- operator provisioned one for them and paid the reserve.
    address_source text        CHECK (address_source IN ('linked', 'provisioned')),
    address_verified_at timestamptz,

    status         text        NOT NULL DEFAULT 'active'
                               CHECK (status IN ('active', 'suspended', 'removed')),
    created_at     timestamptz NOT NULL DEFAULT now(),

    -- An address is only ever recorded together with the proof that produced
    -- it. Without this, a bug that wrote the address but not the timestamp
    -- would leave an unverified address indistinguishable from a verified one.
    CONSTRAINT members_address_needs_proof
        CHECK ((address IS NULL) = (address_verified_at IS NULL)),
    CONSTRAINT members_address_needs_source
        CHECK ((address IS NULL) = (address_source IS NULL)),

    CONSTRAINT members_id_org_key UNIQUE (id, org_id)
);

-- One address per member per org, and one member per address per org: two
-- members sharing an address makes "who approved this" unanswerable.
--
-- Deliberately not globally unique. The same human legitimately belongs to
-- several DAOs with the same wallet, and a global constraint would let the
-- first org they joined lock them out of the second.
CREATE UNIQUE INDEX members_org_address_key
    ON members (org_id, address) WHERE address IS NOT NULL;

CREATE INDEX members_org_idx ON members (org_id);


-- The chat accounts that speak for a member. One human, many handles.
CREATE TABLE user_identities (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    member_id       bigint      NOT NULL,
    org_id          bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    channel         text        NOT NULL CHECK (channel IN ('telegram', 'discord')),
    -- The platform's own immutable user id.
    --
    -- Never the @handle: handles are renameable and, once released,
    -- reassignable to someone else, so binding authority to one would let a
    -- recycled username inherit a role.
    channel_user_id text        NOT NULL CHECK (channel_user_id <> ''),
    handle          text        NOT NULL DEFAULT '',
    linked_at       timestamptz NOT NULL DEFAULT now(),
    -- The identity that authorised adding this one, for the audit trail.
    linked_by       bigint      REFERENCES user_identities (id),

    -- Composite: an identity is always in the same org as its member. This is
    -- the pattern used by every child table here — it costs one redundant
    -- unique index on the parent and makes tenant containment a database fact
    -- rather than a join the application has to remember.
    FOREIGN KEY (member_id, org_id) REFERENCES members (id, org_id) ON DELETE CASCADE
);

-- Within one org, one chat account speaks for exactly one member.
CREATE UNIQUE INDEX user_identities_org_channel_user_key
    ON user_identities (org_id, channel, channel_user_id);

CREATE INDEX user_identities_member_idx ON user_identities (member_id);


-- What a member may ask the bot to do.
--
-- Worth stating plainly, because the distinction is the whole security model:
-- a role decides who the bot will *talk to*. It does not decide who can
-- *spend*. 'approver' means the bot will offer someone the approval link;
-- whether that approval counts is decided by the network against the treasury
-- account's own signer list and thresholds, which this process cannot
-- influence.
CREATE TABLE member_roles (
    org_id     bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    member_id  bigint      NOT NULL,
    role       text        NOT NULL CHECK (role IN
                   ('observer', 'proposer', 'approver', 'admin')),
    granted_at timestamptz NOT NULL DEFAULT now(),
    granted_by bigint,

    PRIMARY KEY (org_id, member_id, role),
    FOREIGN KEY (member_id, org_id) REFERENCES members (id, org_id) ON DELETE CASCADE
);


-- Optional automatic grant: holding a platform role confers an org role.
CREATE TABLE role_bindings (
    org_id       bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    channel      text        NOT NULL CHECK (channel IN ('telegram', 'discord')),
    -- Discord: a role snowflake. Telegram: 'creator' or 'administrator'.
    channel_role text        NOT NULL CHECK (channel_role <> ''),
    -- Note what is missing: 'admin'.
    --
    -- A Discord guild administrator can create a role and assign it to
    -- themselves at will, so letting a platform role confer org administration
    -- would make every guild admin a stelfin admin by construction. Admin is
    -- granted explicitly by an existing admin, and recorded with who did it.
    role         text        NOT NULL CHECK (role IN
                     ('observer', 'proposer', 'approver')),
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (org_id, channel, channel_role, role)
);

-- +goose Down

DROP TABLE role_bindings;
DROP TABLE member_roles;
DROP TABLE user_identities;
DROP TABLE members;
