-- +goose Up

-- Let a contract be a tracked address.
--
-- A DAO whose treasury is a Soroban contract holds its money at a C-address,
-- and until now this table could not name one. The consequence was not a
-- refusal anywhere visible — it was that such a treasury's balance read as
-- permanently zero, because nothing could be tracked and therefore nothing
-- could be ingested.
--
-- org_treasuries has always accepted both prefixes. This brings the address
-- table into line with it, so the account a DAO proved control of is the same
-- account ingestion watches.
ALTER TABLE tracked_addresses
    DROP CONSTRAINT tracked_addresses_address_check;

ALTER TABLE tracked_addresses
    ADD CONSTRAINT tracked_addresses_address_check
        CHECK (address ~ '^[GC][A-Z2-7]{55}$');

-- +goose Down

-- Down is lossy by nature: a contract address recorded while this was up
-- cannot satisfy the narrower constraint. Removing those rows first is
-- deliberate and stated rather than left for the constraint to discover — a
-- migration that fails half way is worse than one that says what it will do.
DELETE FROM tracked_addresses WHERE address LIKE 'C%';

ALTER TABLE tracked_addresses
    DROP CONSTRAINT tracked_addresses_address_check;

ALTER TABLE tracked_addresses
    ADD CONSTRAINT tracked_addresses_address_check
        CHECK (address ~ '^G[A-Z2-7]{55}$');
