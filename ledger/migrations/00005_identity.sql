-- +goose Up

-- Provisioning policy, per org.
--
-- Both default to refusing. A sponsored account costs the operator 1 XLM of
-- base reserve plus 0.5 per trustline, permanently, and chat accounts are free
-- to create — so a deployment anyone can add a bot to is a deployment anyone can
-- spend XLM from, unless the default is off.
--
-- WhatsApp priced this out for us by making Meta verify a phone number first.
-- Nothing does that here, so the gate is explicit: an admin turns provisioning
-- on, and only after the workspace has proved control of a treasury.
ALTER TABLE orgs
    ADD COLUMN enroll_budget_stroops bigint NOT NULL DEFAULT 0
        CHECK (enroll_budget_stroops >= 0),
    ADD COLUMN enroll_daily_cap int NOT NULL DEFAULT 25
        CHECK (enroll_daily_cap >= 0);


-- SEP-10 challenges awaiting a signature.
--
-- A challenge transaction is built with sequence number zero, which makes it
-- structurally unsubmittable: it can be signed and checked, and it can never
-- move anything. That is what makes it safe to hand someone a link that asks
-- them to sign with the key that holds their money.
CREATE TABLE auth_challenges (
    -- The challenge transaction's hash, and the lookup key.
    hash         text        PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    org_id       bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,

    -- Bound to one chat identity. A challenge issued to one account may not be
    -- completed by another, even for the same address — otherwise anyone who
    -- learned a member's public address could attach their own chat account to
    -- that member and inherit their role.
    identity_id  bigint      NOT NULL REFERENCES user_identities (id) ON DELETE CASCADE,

    purpose      text        NOT NULL CHECK (purpose IN ('link_member', 'link_treasury')),
    address      text        NOT NULL CHECK (address ~ '^G[A-Z2-7]{55}$'),
    envelope_xdr text        NOT NULL CHECK (envelope_xdr <> ''),

    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,

    CONSTRAINT auth_challenges_expires_after_creation CHECK (expires_at > created_at)
);

-- One live challenge per identity per purpose. Asking again supersedes the
-- previous attempt rather than accumulating orphans — the same shape, and the
-- same reasoning, as pending_enrollments.
CREATE UNIQUE INDEX auth_challenges_live_key
    ON auth_challenges (identity_id, purpose) WHERE consumed_at IS NULL;

CREATE INDEX auth_challenges_expiry_idx
    ON auth_challenges (expires_at) WHERE consumed_at IS NULL;


-- Short codes that attach a second chat account to an existing member.
--
-- This path can only ever *add a handle* to a member who has already proved an
-- address. It can never create an address or change one, which is what makes it
-- safe to be a shared secret rather than a signature: the worst a stolen code
-- can do is let someone speak as a member who still cannot sign anything.
CREATE TABLE identity_link_codes (
    code        text        PRIMARY KEY CHECK (code ~ '^[A-Z2-9]{8}$'),
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    member_id   bigint      NOT NULL,
    issued_by   bigint      NOT NULL REFERENCES user_identities (id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    consumed_by bigint      REFERENCES user_identities (id),

    FOREIGN KEY (member_id, org_id) REFERENCES members (id, org_id) ON DELETE CASCADE,
    CONSTRAINT identity_link_codes_expires_after_creation CHECK (expires_at > created_at)
);

-- One live code per member: a second request supersedes the first, so a code
-- read aloud in a channel and then regretted can be replaced immediately.
CREATE UNIQUE INDEX identity_link_codes_live_key
    ON identity_link_codes (member_id) WHERE consumed_at IS NULL;

CREATE INDEX identity_link_codes_expiry_idx
    ON identity_link_codes (expires_at) WHERE consumed_at IS NULL;


-- Accounts the operator has paid to bring into existence.
--
-- The cost is real and not recoverable by wishing: 1 XLM of base reserve plus
-- 0.5 per trustline, locked for as long as the account exists. This table is
-- what makes that cost countable — per member, per org, per day, and in total.
CREATE TABLE enrollment_grants (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id       bigint      NOT NULL REFERENCES orgs (id),
    identity_id  bigint      NOT NULL REFERENCES user_identities (id),
    member_id    bigint      NOT NULL,
    address      text        NOT NULL CHECK (address ~ '^G[A-Z2-7]{55}$'),

    -- Stroops of XLM committed to this account's reserves.
    --
    -- Stored rather than recomputed, so a later reclaim knows what it is
    -- releasing and a change to the trustline set does not retroactively
    -- rewrite what was spent.
    reserve_cost bigint      NOT NULL CHECK (reserve_cost > 0),

    granted_at   timestamptz NOT NULL DEFAULT now(),
    reclaimed_at timestamptz,

    FOREIGN KEY (member_id, org_id) REFERENCES members (id, org_id)
);

-- One grant per member, ever. A hard cap rather than a rate limit: nobody needs
-- the operator to pay for a second account for them.
CREATE UNIQUE INDEX enrollment_grants_member_key ON enrollment_grants (member_id);

CREATE INDEX enrollment_grants_org_time_idx ON enrollment_grants (org_id, granted_at DESC);
CREATE INDEX enrollment_grants_time_idx ON enrollment_grants (granted_at DESC);

-- +goose Down

DROP TABLE enrollment_grants;
DROP TABLE identity_link_codes;
DROP TABLE auth_challenges;
ALTER TABLE orgs
    DROP COLUMN enroll_daily_cap,
    DROP COLUMN enroll_budget_stroops;
