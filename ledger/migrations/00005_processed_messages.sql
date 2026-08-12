-- +goose Up

-- Every inbound WhatsApp message this server has already acted on.
--
-- Meta retries a delivery that is slow or fails, and the same message id
-- arrives again. Without this, one message could start two payment flows and
-- the user would be shown two confirmations for one instruction.
--
-- The message id is Meta's (`wamid...`), which is stable across retries. It is
-- the primary key, so claiming a message is an insert that either succeeds
-- once or conflicts — there is no read-then-write window for a concurrent
-- retry to slip through.
CREATE TABLE processed_messages (
    id          text        PRIMARY KEY CHECK (id <> ''),
    -- Who sent it, for the audit trail. Not used for lookup.
    sender      text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX processed_messages_received_at_idx ON processed_messages (received_at);

-- +goose Down

DROP TABLE processed_messages;
