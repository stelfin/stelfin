package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

// TestNoCrossOrgReads is the test this package exists to make possible.
//
// It builds two tenants with deliberately similar data, then calls every read
// method with one org's id and the *other* org's row id. Each must return
// nothing — not an error, not a partial answer, and above all not the other
// tenant's data.
//
// A method added later without an org predicate fails here rather than in
// production, where the symptom would not be an error at all: it would be one
// DAO seeing another DAO's balance and having no way to tell.
func TestNoCrossOrgReads(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()

	mine := newTenant(t, s, "-200001")
	theirs := newTenant(t, s, "-200002")

	// Give the other tenant something worth leaking.
	theirs.deposit(t, s, t.Name()+"/theirs", money.MustParse("999"))
	theirAddress := keypair.MustRandom().Address()
	must(t, s.SetMemberAddress(ctx, theirs.org.ID, theirs.member.ID, theirAddress, AddressLinked),
		"give the other member an address")
	must(t, s.GrantRole(ctx, theirs.org.ID, theirs.member.ID, chat.RoleAdmin, 0),
		"give the other member a role")

	t.Run("Member", func(t *testing.T) {
		_, err := s.Member(ctx, mine.org.ID, theirs.member.ID)
		if !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("read another org's member: err = %v, want ErrMemberNotFound", err)
		}
	})

	t.Run("MemberByIdentity", func(t *testing.T) {
		// The other tenant's chat identity, asked for inside this org.
		_, ok, err := s.MemberByIdentity(ctx, mine.org.ID, chat.Telegram, "u-200002")
		must(t, err, "member by identity")
		if ok {
			t.Fatal("another org's chat identity resolved to a member here")
		}
	})

	t.Run("Balance", func(t *testing.T) {
		got, err := s.Balance(ctx, mine.org.ID, theirs.account, theirs.usdc)
		must(t, err, "balance")
		if !got.IsZero() {
			t.Fatalf("read %s from another org's account", got)
		}
	})

	t.Run("Balances", func(t *testing.T) {
		got, err := s.Balances(ctx, mine.org.ID, theirs.account)
		must(t, err, "balances")
		if len(got) != 0 {
			t.Fatalf("read %d balance rows from another org's account", len(got))
		}
	})

	t.Run("TreasurySnapshot", func(t *testing.T) {
		// Scoped by org alone, so the check is that this org's snapshot never
		// contains the other's treasury account.
		got, err := s.TreasurySnapshot(ctx, mine.org.ID)
		must(t, err, "treasury snapshot")
		for _, line := range got {
			if line.Account == theirs.treasury {
				t.Fatalf("another org's treasury appeared in this org's snapshot: %+v", line)
			}
		}
	})

	t.Run("History", func(t *testing.T) {
		rows, _, err := s.History(ctx, mine.org.ID, HistoryFilter{Account: theirs.account})
		must(t, err, "history")
		if len(rows) != 0 {
			t.Fatalf("read %d history rows from another org's account", len(rows))
		}

		// And unfiltered: this org's history must never mention the other's
		// accounts, which is the case a per-account predicate would miss.
		all, _, err := s.History(ctx, mine.org.ID, HistoryFilter{Limit: MaxHistoryLimit})
		must(t, err, "unfiltered history")
		for _, r := range all {
			if r.Account == theirs.account || r.Account == theirs.external {
				t.Fatalf("another org's account appeared in this org's history: %+v", r)
			}
		}
	})

	t.Run("AccountFor", func(t *testing.T) {
		_, ok, err := s.AccountFor(ctx, mine.org.ID, "telegram:u-200002")
		must(t, err, "account for")
		if ok {
			t.Fatal("another org's owner reference resolved to an account here")
		}
	})

	t.Run("AddressForOwner", func(t *testing.T) {
		_, err := s.AddressForOwner(ctx, mine.org.ID, "telegram:u-200002")
		if !errors.Is(err, ErrAddressNotTracked) {
			t.Fatalf("err = %v, want ErrAddressNotTracked", err)
		}
	})

	t.Run("HasAddress", func(t *testing.T) {
		got, err := s.HasAddress(ctx, mine.org.ID, "telegram:u-200002")
		must(t, err, "has address")
		if got {
			t.Fatal("another org's owner reported as having an address here")
		}
	})

	t.Run("EffectiveRole", func(t *testing.T) {
		got, err := s.EffectiveRole(ctx, mine.org.ID, theirs.member.ID, chat.Telegram, nil)
		must(t, err, "effective role")
		if got != chat.RoleNone {
			t.Fatalf("another org's member holds %s here, want none", got)
		}
	})

	t.Run("Org", func(t *testing.T) {
		// Org is looked up by its own id, so there is nothing to cross — but the
		// space lookup that precedes every message must not leak.
		got, ok, err := s.OrgForSpace(ctx, chat.Telegram, "-200002")
		must(t, err, "org for space")
		if !ok || got.ID != theirs.org.ID {
			t.Fatalf("space resolution is wrong: %+v", got)
		}
		if got.ID == mine.org.ID {
			t.Fatal("two spaces resolved to the same org")
		}
	})
}

// TestPostRefusesToCrossOrgs: the read side is discipline plus this test; the
// write side is a constraint. A posting into another tenant's account is
// refused by the database whatever this package does.
func TestPostRefusesToCrossOrgs(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()

	mine := newTenant(t, s, "-200003")
	theirs := newTenant(t, s, "-200004")

	_, err := s.Post(ctx, ledger.PostRequest{
		Org:            mine.org.ID,
		IdempotencyKey: t.Name(),
		Kind:           ledger.TxSend,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []ledger.Posting{
			{Account: theirs.account, Asset: mine.usdc, Amount: money.MustParse("100")},
			{Account: mine.external, Asset: mine.usdc, Amount: money.MustParse("-100")},
		},
	})
	if !errors.Is(err, ledger.ErrCrossOrg) {
		t.Fatalf("error = %v, want ErrCrossOrg", err)
	}
}
