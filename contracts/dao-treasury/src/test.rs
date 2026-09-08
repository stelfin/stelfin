#![cfg(test)]

//! Tests for the treasury.
//!
//! A note on `mock_all_auths`, because it is the difference between a suite
//! that proves something and one that proves nothing.
//!
//! `mock_all_auths` makes every `require_auth` succeed. Under it, a contract
//! that had no authorisation checks at all would pass every test in this file
//! except the ones that assert a specific caller. So the checks that matter —
//! only the admin may change policy, only a member may vote, and `execute`
//! deliberately needs nobody — are asserted with `mock_auths`, which grants
//! exactly one signature and nothing else, or with no mocking at all.
//!
//! Convenience mocking is used only where the test is about arithmetic and the
//! authorisation is not what is under examination.

use soroban_sdk::testutils::{Address as _, AuthorizedFunction, Ledger};
use soroban_sdk::{testutils::MockAuth, testutils::MockAuthInvoke};
use soroban_sdk::{token, Address, Env, IntoVal, Symbol};

use crate::types::{Error, Policy, Status};
use crate::{DaoTreasury, DaoTreasuryClient};

const DAY: u64 = 86_400;

/// A policy that passes a proposal when more than half of a quorum agrees, with
/// a day of voting and a day of timelock, and no auto-spend.
fn strict() -> Policy {
    Policy {
        quorum_bps: 5_000,
        approval_bps: 6_000,
        voting_period: DAY,
        timelock: DAY,
        spend_limit: 0,
        spend_window: 0,
    }
}

struct Fixture<'a> {
    env: Env,
    client: DaoTreasuryClient<'a>,
    contract: Address,
    admin: Address,
    token: Address,
    token_admin: token::StellarAssetClient<'a>,
    ada: Address,
    bo: Address,
    cy: Address,
    payee: Address,
}

fn setup(policy: Policy) -> Fixture<'static> {
    let env = Env::default();
    env.ledger().set_timestamp(1_700_000_000);

    let admin = Address::generate(&env);
    let contract = env.register(DaoTreasury, ());
    let client = DaoTreasuryClient::new(&env, &contract);

    let issuer = Address::generate(&env);
    let asset = env.register_stellar_asset_contract_v2(issuer);
    let token = asset.address();
    let token_admin = token::StellarAssetClient::new(&env, &token);

    let ada = Address::generate(&env);
    let bo = Address::generate(&env);
    let cy = Address::generate(&env);
    let payee = Address::generate(&env);

    // The set-up itself is done under blanket mocking: what is under test below
    // is the contract's behaviour, not whether a fixture can sign.
    env.mock_all_auths();
    client.init(&admin, &policy);
    client.set_member(&ada, &1);
    client.set_member(&bo, &1);
    client.set_member(&cy, &1);
    client.allow_recipient(&payee);
    token_admin.mint(&contract, &1_000_000);

    Fixture {
        env,
        client,
        contract,
        admin,
        token,
        token_admin,
        ada,
        bo,
        cy,
        payee,
    }
}

fn memo(env: &Env) -> soroban_sdk::BytesN<32> {
    soroban_sdk::BytesN::from_array(env, &[7u8; 32])
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

#[test]
fn init_records_the_policy_and_refuses_a_second_time() {
    let f = setup(strict());
    assert_eq!(f.client.policy(), strict());

    // A second init would let whoever called it replace the admin.
    let err = f
        .client
        .try_init(&f.admin, &strict())
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::AlreadyInitialised);
}

#[test]
fn a_policy_that_could_never_pass_or_would_always_pass_is_refused() {
    let env = Env::default();
    let admin = Address::generate(&env);
    let contract = env.register(DaoTreasury, ());
    let client = DaoTreasuryClient::new(&env, &contract);
    env.mock_all_auths();

    let cases = [
        // A treasury that can never reach quorum again, discovered at the worst
        // possible moment.
        Policy {
            quorum_bps: 0,
            ..strict()
        },
        Policy {
            quorum_bps: 10_001,
            ..strict()
        },
        // At exactly half, a tie passes. Nobody describes their treasury that
        // way.
        Policy {
            approval_bps: 5_000,
            ..strict()
        },
        Policy {
            approval_bps: 4_000,
            ..strict()
        },
        Policy {
            approval_bps: 10_001,
            ..strict()
        },
        // Voting that closes the instant it opens.
        Policy {
            voting_period: 0,
            ..strict()
        },
        // A limit that never resets: the first payment consumes it forever.
        Policy {
            spend_limit: 100,
            spend_window: 0,
            ..strict()
        },
        Policy {
            spend_limit: -1,
            ..strict()
        },
    ];

    for policy in cases {
        let err = client.try_init(&admin, &policy).err().unwrap().unwrap();
        assert_eq!(err, Error::InvalidPolicy, "accepted {policy:?}");
    }
}

/// Only the admin may change policy — asserted without blanket mocking.
///
/// The whole test: a member with real signing weight, who can vote and propose,
/// still cannot rewrite the rules.
#[test]
fn only_the_admin_changes_policy() {
    let f = setup(strict());
    let loose = Policy {
        timelock: 0,
        ..strict()
    };

    // Ada's signature, and only Ada's.
    f.env.mock_auths(&[MockAuth {
        address: &f.ada,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "set_policy",
            args: (loose.clone(),).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    let attempt = f.client.try_set_policy(&loose);
    assert!(attempt.is_err(), "a member rewrote the treasury's rules");

    // The policy is unchanged, which is the fact that matters.
    assert_eq!(f.client.policy(), strict());
}

/// Only the admin may change membership, likewise.
#[test]
fn only_the_admin_changes_membership() {
    let f = setup(strict());
    let intruder = Address::generate(&f.env);

    f.env.mock_auths(&[MockAuth {
        address: &f.ada,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "set_member",
            args: (intruder.clone(), 100u32).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    let attempt = f.client.try_set_member(&intruder, &100);
    assert!(attempt.is_err(), "a member added a member");
    assert_eq!(f.client.member_weight(&intruder), 0);
}

#[test]
fn removing_the_last_member_is_refused() {
    let f = setup(strict());
    f.env.mock_all_auths();

    f.client.set_member(&f.bo, &0);
    f.client.set_member(&f.cy, &0);
    assert_eq!(f.client.total_weight(), 1);

    // The one operation that can make a treasury permanently unable to spend,
    // with no recourse and no signal until somebody tries.
    let err = f.client.try_set_member(&f.ada, &0).err().unwrap().unwrap();
    assert_eq!(err, Error::WouldLockOut);
    assert_eq!(f.client.total_weight(), 1);
}

// ---------------------------------------------------------------------------
// Proposals
// ---------------------------------------------------------------------------

#[test]
fn only_a_member_proposes() {
    let f = setup(strict());
    let stranger = Address::generate(&f.env);
    f.env.mock_all_auths();

    let err = f
        .client
        .try_propose(&stranger, &f.token, &f.payee, &100, &memo(&f.env))
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::NotMember);
}

/// A proposer must actually sign, not merely be a member.
///
/// Without blanket mocking: Bo is a member, and a proposal in Bo's name signed
/// by Ada is a proposal Bo did not make.
#[test]
fn a_proposal_needs_its_proposers_signature() {
    let f = setup(strict());

    f.env.mock_auths(&[MockAuth {
        address: &f.ada,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "propose",
            args: (
                f.bo.clone(),
                f.token.clone(),
                f.payee.clone(),
                100i128,
                memo(&f.env),
            )
                .into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    let attempt = f
        .client
        .try_propose(&f.bo, &f.token, &f.payee, &100, &memo(&f.env));
    assert!(attempt.is_err(), "one member proposed in another's name");
}

#[test]
fn a_recipient_off_the_allowlist_is_refused_at_proposal_time() {
    let f = setup(strict());
    let stranger = Address::generate(&f.env);
    f.env.mock_all_auths();

    // Checked here as well as at execution, so a DAO does not spend a week
    // voting on a payment that was never going to be possible.
    let err = f
        .client
        .try_propose(&f.ada, &f.token, &stranger, &100, &memo(&f.env))
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::RecipientNotAllowed);
}

#[test]
fn a_member_votes_once() {
    let f = setup(strict());
    f.env.mock_all_auths();

    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));
    f.client.vote(&f.ada, &id, &true);

    let err = f
        .client
        .try_vote(&f.ada, &id, &false)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::AlreadyVoted);
    assert_eq!(f.client.proposal(&id).yes, 1);
}

/// A vote needs the voter's own signature.
#[test]
fn a_vote_needs_the_voters_signature() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));

    f.env.mock_auths(&[MockAuth {
        address: &f.ada,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "vote",
            args: (f.bo.clone(), id, true).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    let attempt = f.client.try_vote(&f.bo, &id, &true);
    assert!(attempt.is_err(), "one member voted as another");
    assert_eq!(f.client.proposal(&id).yes, 0);
}

#[test]
fn voting_closes() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));

    f.env.ledger().set_timestamp(1_700_000_000 + DAY);
    let err = f
        .client
        .try_vote(&f.ada, &id, &true)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::NotOpen);
}

#[test]
fn quorum_and_approval_are_two_different_questions() {
    // Three members of weight one. Quorum needs 50% of 3, rounded up: 2 votes.
    // Approval needs 60% of what was cast, rounded up.
    let f = setup(strict());
    f.env.mock_all_auths();

    // One vote in favour: unanimous among those who turned up, and short of
    // quorum. A treasury with three active members out of thirty that
    // collapsed these into one number would be passing everything.
    let lonely = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));
    f.client.vote(&f.ada, &lonely, &true);
    f.env.ledger().set_timestamp(1_700_000_000 + DAY);
    assert_eq!(f.client.tally(&lonely), Status::Rejected);

    // Two votes, one each way: quorum met, approval not — 1 of 2 is 50%, and
    // 60% of 2 rounded up is 2.
    let split = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));
    f.client.vote(&f.ada, &split, &true);
    f.client.vote(&f.bo, &split, &false);
    f.env.ledger().set_timestamp(1_700_000_000 + 2 * DAY + 1);
    assert_eq!(f.client.tally(&split), Status::Rejected);
}

#[test]
fn a_proposal_that_passes_can_be_executed_after_its_timelock() {
    let f = setup(strict());
    f.env.mock_all_auths();

    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));
    f.client.vote(&f.ada, &id, &true);
    f.client.vote(&f.bo, &id, &true);

    // Voting is still open.
    let err = f.client.try_execute(&id).err().unwrap().unwrap();
    assert_eq!(err, Error::NotExecutable);

    // Voting has closed and it passed, but the timelock has not run. This is
    // the window in which the rest of the DAO gets to notice.
    f.env.ledger().set_timestamp(1_700_000_000 + DAY);
    assert_eq!(f.client.tally(&id), Status::Passed);
    let err = f.client.try_execute(&id).err().unwrap().unwrap();
    assert_eq!(err, Error::NotExecutable);

    f.env.ledger().set_timestamp(1_700_000_000 + 2 * DAY);
    f.client.execute(&id);

    assert_eq!(f.client.proposal(&id).status, Status::Executed);
    let token = token::Client::new(&f.env, &f.token);
    assert_eq!(token.balance(&f.payee), 100);
    assert_eq!(token.balance(&f.contract), 999_900);
}

/// The headline: `execute` requires nobody's signature.
///
/// Asserted with authorisation explicitly emptied, not merely unmocked, so this
/// cannot pass because of a fixture. A contract whose funds need a specific
/// person online is a contract with a hostage — and stelfin's uptime must never
/// be a condition of a DAO spending its own money.
#[test]
fn execute_needs_nobodys_signature() {
    let f = setup(strict());
    f.env.mock_all_auths();

    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));
    f.client.vote(&f.ada, &id, &true);
    f.client.vote(&f.bo, &id, &true);
    f.env.ledger().set_timestamp(1_700_000_000 + 2 * DAY);

    // No signatures available at all. Anyone at all can carry this out.
    f.env.set_auths(&[]);
    f.client.execute(&id);

    assert_eq!(f.client.proposal(&id).status, Status::Executed);
    assert_eq!(token::Client::new(&f.env, &f.token).balance(&f.payee), 100);
}

#[test]
fn a_proposal_executes_once() {
    let f = setup(strict());
    f.env.mock_all_auths();

    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));
    f.client.vote(&f.ada, &id, &true);
    f.client.vote(&f.bo, &id, &true);
    f.env.ledger().set_timestamp(1_700_000_000 + 2 * DAY);
    f.client.execute(&id);

    let err = f.client.try_execute(&id).err().unwrap().unwrap();
    assert_eq!(err, Error::NotExecutable);
    assert_eq!(token::Client::new(&f.env, &f.token).balance(&f.payee), 100);
}

/// Removing a recipient during the timelock stops the payment.
///
/// This is what the timelock is for. A proposal that passed is not a promise
/// that the world has not changed since.
#[test]
fn a_recipient_removed_during_the_timelock_is_not_paid() {
    let f = setup(strict());
    f.env.mock_all_auths();

    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));
    f.client.vote(&f.ada, &id, &true);
    f.client.vote(&f.bo, &id, &true);

    f.env.ledger().set_timestamp(1_700_000_000 + DAY);
    assert_eq!(f.client.tally(&id), Status::Passed);
    f.client.deny_recipient(&f.payee);

    f.env.ledger().set_timestamp(1_700_000_000 + 2 * DAY);
    let err = f.client.try_execute(&id).err().unwrap().unwrap();
    assert_eq!(err, Error::RecipientNotAllowed);
    assert_eq!(token::Client::new(&f.env, &f.token).balance(&f.payee), 0);
}

#[test]
fn only_its_proposer_or_the_admin_cancels() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f
        .client
        .propose(&f.ada, &f.token, &f.payee, &100, &memo(&f.env));

    // Bo is a member and cannot cancel Ada's proposal: that would turn a vote
    // into a race.
    f.env.mock_auths(&[MockAuth {
        address: &f.bo,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "cancel",
            args: (id,).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    let attempt = f.client.try_cancel(&id);
    assert!(attempt.is_err(), "a member cancelled another's proposal");
    assert_eq!(f.client.proposal(&id).status, Status::Open);

    f.env.mock_all_auths();
    f.client.cancel(&id);
    assert_eq!(f.client.proposal(&id).status, Status::Cancelled);
}

// ---------------------------------------------------------------------------
// The auto-spend window
// ---------------------------------------------------------------------------

#[test]
fn auto_spend_is_off_until_a_treasury_turns_it_on() {
    let f = setup(strict());
    f.env.mock_all_auths();

    // The default. Every payment is a proposal.
    assert_eq!(f.client.spendable(), 0);
    let err = f
        .client
        .try_spend(&f.ada, &f.token, &f.payee, &1)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::SpendLimitExceeded);
}

#[test]
fn the_spend_window_rolls_over() {
    let f = setup(Policy {
        spend_limit: 500,
        spend_window: DAY,
        ..strict()
    });
    f.env.mock_all_auths();
    let token = token::Client::new(&f.env, &f.token);

    f.client.spend(&f.ada, &f.token, &f.payee, &300);
    assert_eq!(f.client.spendable(), 200);
    assert_eq!(token.balance(&f.payee), 300);

    // Over the limit, in one go and in aggregate.
    let err = f
        .client
        .try_spend(&f.ada, &f.token, &f.payee, &201)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::SpendLimitExceeded);
    assert_eq!(token.balance(&f.payee), 300);

    f.client.spend(&f.ada, &f.token, &f.payee, &200);
    assert_eq!(f.client.spendable(), 0);

    // A new window opens a day after the first payment, not on a clock
    // boundary — aligning to one would let a caller wait for the tick and
    // spend two windows back to back.
    f.env.ledger().set_timestamp(1_700_000_000 + DAY + 1);
    assert_eq!(f.client.spendable(), 500);
    f.client.spend(&f.ada, &f.token, &f.payee, &500);
    assert_eq!(token.balance(&f.payee), 1_000);
}

/// Spending needs a member's own signature.
#[test]
fn a_spend_needs_the_members_signature() {
    let f = setup(Policy {
        spend_limit: 500,
        spend_window: DAY,
        ..strict()
    });

    f.env.mock_auths(&[MockAuth {
        address: &f.cy,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "spend",
            args: (f.ada.clone(), f.token.clone(), f.payee.clone(), 100i128).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    let attempt = f.client.try_spend(&f.ada, &f.token, &f.payee, &100);
    assert!(attempt.is_err(), "one member spent in another's name");
    assert_eq!(token::Client::new(&f.env, &f.token).balance(&f.payee), 0);
}

#[test]
fn a_spend_to_an_unlisted_recipient_is_refused() {
    let f = setup(Policy {
        spend_limit: 500,
        spend_window: DAY,
        ..strict()
    });
    f.env.mock_all_auths();
    let stranger = Address::generate(&f.env);

    let err = f
        .client
        .try_spend(&f.ada, &f.token, &stranger, &1)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::RecipientNotAllowed);
}

/// The admin's signature is required, and it is required for that exact call.
///
/// Asserted from the recorded authorisation rather than from "it did not fail",
/// so a check that happened to require some other signature would still be
/// caught.
#[test]
fn the_admin_signature_is_recorded_against_the_call_it_authorised() {
    let f = setup(strict());
    let newcomer = Address::generate(&f.env);

    f.env.mock_auths(&[MockAuth {
        address: &f.admin,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "set_member",
            args: (newcomer.clone(), 2u32).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    f.client.set_member(&newcomer, &2);

    // Read before anything else is called. auths() reports the most recent
    // invocation, so a read in between would clear it and this test would
    // assert nothing.
    let auths = f.env.auths();
    assert_eq!(auths.len(), 1, "one signature, from one signer");
    let (who, invocation) = &auths[0];
    assert_eq!(who, &f.admin);
    match &invocation.function {
        AuthorizedFunction::Contract((contract, function, args)) => {
            assert_eq!(contract, &f.contract);
            assert_eq!(function, &Symbol::new(&f.env, "set_member"));
            assert_eq!(args, &(newcomer.clone(), 2u32).into_val(&f.env));
        }
        other => panic!("authorised something else: {other:?}"),
    }
    assert!(
        invocation.sub_invocations.is_empty(),
        "the admin authorised a call that reached further than the one it named"
    );

    assert_eq!(f.client.member_weight(&newcomer), 2);
    let _ = &f.token_admin;
}
