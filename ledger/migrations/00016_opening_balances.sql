-- +goose Up

-- An opening balance: money an account turned out to hold that these books
-- never saw arrive.
--
-- The books are an index of what the network did since this deployment began
-- watching an address, and most addresses were not empty when it began. A
-- wallet a member links has a history. A treasury held by a contract may have
-- been paid before anybody here knew of it. The first time such an account
-- pays out, the network has already let it, and the books say it could not
-- have: it would go negative.
--
-- The network is right and the books are short. So the difference is written
-- down as what it is — an opening balance, from outside, dated when it was
-- found — and the payment is then recorded like any other. It gets a kind of
-- its own so that nobody reading the history takes it for a deposit that
-- happened on that day.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check CHECK (kind IN (
    'deposit', 'send', 'fee', 'sponsor', 'reserve_release',
    'withdrawal', 'trade', 'opening'));

-- +goose Down

-- Lossy, and said to be: an opening balance recorded while this was up cannot
-- satisfy the narrower constraint. Recorded as deposits rather than deleted,
-- because deleting them would leave the accounts they opened below zero.
UPDATE ledger_transactions SET kind = 'deposit' WHERE kind = 'opening';
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check CHECK (kind IN (
    'deposit', 'send', 'fee', 'sponsor', 'reserve_release',
    'withdrawal', 'trade'));
