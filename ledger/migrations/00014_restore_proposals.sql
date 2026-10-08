-- +goose Up

-- A restore is a proposal of its own.
--
-- When a contract call finds the state it needs archived, bringing it back is a
-- separate transaction that costs money and changes nothing else. It is put to
-- the treasury's signers like anything else the treasury pays for, and is not
-- folded quietly into the call that needed it.
ALTER TABLE proposals DROP CONSTRAINT proposals_kind_check;
ALTER TABLE proposals ADD CONSTRAINT proposals_kind_check CHECK (kind IN (
    'payment', 'payroll', 'trustline', 'dex', 'signers', 'invoke', 'restore'));

-- +goose Down

DELETE FROM proposals WHERE kind = 'restore';
ALTER TABLE proposals DROP CONSTRAINT proposals_kind_check;
ALTER TABLE proposals ADD CONSTRAINT proposals_kind_check CHECK (kind IN (
    'payment', 'payroll', 'trustline', 'dex', 'signers', 'invoke'));
