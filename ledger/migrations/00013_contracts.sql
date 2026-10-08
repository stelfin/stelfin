-- +goose Up

-- Third-party contracts a workspace allows its treasury to call, each pinned
-- to the code it runs.
--
-- A contract address is not a promise about behaviour: the code behind it can
-- be replaced, at the same address, by whoever the contract lets upgrade it.
-- So what is allowed is an address *and* what it was running when it was
-- allowed. Every call is checked against that before it is built and again
-- before it is submitted, and a contract whose code has changed is refused
-- until somebody looks at it and allows it again.
CREATE TABLE allowed_contracts (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id      bigint      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    label       text        NOT NULL CHECK (label ~ '^[a-z][a-z0-9-]{0,30}$'),
    address     text        NOT NULL CHECK (address ~ '^C[A-Z2-7]{55}$'),

    -- 'wasm' for uploaded code, pinned by its hash; 'stellar_asset' for the
    -- built-in contract of a classic asset, which has no code to change.
    executable  text        NOT NULL CHECK (executable IN ('wasm', 'stellar_asset')),
    wasm_hash   text        NOT NULL DEFAULT ''
                            CHECK ((executable = 'wasm') = (wasm_hash ~ '^[0-9a-f]{64}$')),

    added_by    bigint      NOT NULL,
    added_at    timestamptz NOT NULL DEFAULT now(),
    removed_at  timestamptz,

    FOREIGN KEY (added_by, org_id) REFERENCES members (id, org_id)
);

CREATE UNIQUE INDEX allowed_contracts_live_label_key
    ON allowed_contracts (org_id, label) WHERE removed_at IS NULL;
CREATE UNIQUE INDEX allowed_contracts_live_address_key
    ON allowed_contracts (org_id, address) WHERE removed_at IS NULL;

-- +goose Down

DROP TABLE allowed_contracts;
