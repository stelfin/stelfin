-- +goose Up

-- Tokens that let an agent read a workspace's data over MCP.
--
-- One per workspace per integration, revocable, and never a credential this
-- deployment can reconstruct: only a hash is stored, the same way a password
-- would be. A database dump is then a list of what exists rather than a set of
-- working keys.
--
-- These authorise reading and proposing, and nothing else. There is no token
-- here that can sign or submit — that is not a permission this table can
-- express, which is the point of it not being a column.
CREATE TABLE mcp_tokens (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,

    -- sha256 of the token, hex. The token itself is shown once, when it is
    -- minted, and is not recoverable afterwards.
    token_hash  text        NOT NULL CHECK (token_hash ~ '^[0-9a-f]{64}$'),

    -- What it is for, so a workspace with three integrations can revoke one.
    label       text        NOT NULL CHECK (label <> ''),

    -- The tier this token may reach.
    --
    -- 'read' can observe and nothing more. 'propose' can additionally draft a
    -- payment for a human to approve — it still cannot cause one. Separate
    -- values rather than a boolean because a third tier must be a deliberate
    -- migration and not a flag somebody flips.
    tier        text        NOT NULL DEFAULT 'read' CHECK (tier IN ('read', 'propose')),

    created_by  bigint      REFERENCES user_identities (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Set on first use and on every use after, so a workspace can see which of
    -- its tokens are actually being used before revoking one.
    last_used_at timestamptz,

    -- Revocation is a positive fact with a time on it, not a deleted row.
    -- "Revoked" and "never existed" are different answers to an auditor.
    revoked_at  timestamptz,
    expires_at  timestamptz,

    CONSTRAINT mcp_tokens_expires_after_creation
        CHECK (expires_at IS NULL OR expires_at > created_at)
);

-- The lookup is by hash and must be fast and total: it happens on every
-- request an agent makes.
CREATE UNIQUE INDEX mcp_tokens_hash_key ON mcp_tokens (token_hash);

CREATE INDEX mcp_tokens_org_idx ON mcp_tokens (org_id, created_at DESC);

-- +goose Down

DROP TABLE mcp_tokens;
