-- +goose Up

-- Connector calls, counted where every instance can see them.
--
-- The guard's own counter lives in one process, so two instances of stelfin
-- each allow a grant's full hourly rate. This is the shared count: one row per
-- call, kept for an hour and then pruned by the next call under the same key.
CREATE TABLE connector_calls (
    org_id     bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    connector  text        NOT NULL,
    called_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX connector_calls_window_idx ON connector_calls (org_id, connector, called_at);


-- Drafts an agent made.
--
-- An agent holding a propose-tier token may draft a payroll for a person to
-- review. It may not propose one, and a draft it made has to be recognisable as
-- its work by whoever reviews it: the people signing should know whether the
-- rows came from a colleague's spreadsheet or from a program.
--
-- So a draft has exactly one author, and the schema says which kind.
ALTER TABLE drafts ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE drafts ALTER COLUMN connector_id DROP NOT NULL;
ALTER TABLE drafts ADD COLUMN agent_token_id bigint REFERENCES mcp_tokens (id);
ALTER TABLE drafts ADD CONSTRAINT drafts_one_author
    CHECK ((created_by IS NULL) <> (agent_token_id IS NULL));
-- A member's draft is read from a connector; an agent's is written by the agent.
ALTER TABLE drafts ADD CONSTRAINT drafts_source_matches_author
    CHECK ((agent_token_id IS NULL) = (connector_id IS NOT NULL));

-- +goose Down

DELETE FROM drafts WHERE agent_token_id IS NOT NULL;
ALTER TABLE drafts DROP CONSTRAINT drafts_source_matches_author;
ALTER TABLE drafts DROP CONSTRAINT drafts_one_author;
ALTER TABLE drafts DROP COLUMN agent_token_id;
ALTER TABLE drafts ALTER COLUMN connector_id SET NOT NULL;
ALTER TABLE drafts ALTER COLUMN created_by SET NOT NULL;
DROP TABLE connector_calls;
