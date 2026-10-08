#![cfg(test)]

//! Tests for the treasury.
//!
//! A note on `mock_all_auths`, because it is the difference between a suite
//! that proves something and one that proves nothing.
//!
//! `mock_all_auths` makes every `require_auth` succeed. Under it, a contract
//! that had no authorisation checks at all would pass every test in this file
//! except the ones that assert a specific caller. So the checks that matter —
//! only a member may propose or vote, only its proposer may withdraw a
//! proposal, and `execute` deliberately needs nobody — are asserted with
//! `mock_auths`, which grants exactly one signature and nothing else, or with
//! no mocking at all.
//!
//! It is used the other way round as well, and that is the more important use
//! in this version. The claim this contract makes is that no single key can
//! move the treasury's money or change its rules. The tests for that claim run
//! with every signature granted — every `require_auth` passing, for anybody —
//! and show that it still cannot be done. A claim about what a key cannot do
//! is only tested by handing over all the keys.

extern crate std;

use soroban_sdk::testutils::storage::{Instance as _, Persistent as _};
use soroban_sdk::testutils::{Address as _, Events as _, Ledger};
use soroban_sdk::{testutils::MockAuth, testutils::MockAuthInvoke};
use soroban_sdk::{token, vec, Address, BytesN, Env, IntoVal, Vec};

use crate::policy::KEEP_LEDGERS;
use crate::types::{Action, Error, Member, Payment, Policy, Status};
use crate::{DaoTreasury, DaoTreasuryClient, Key};

const DAY: u64 = 86_400;
const START: u64 = 1_700_000_000;

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
    token: Address,
    ada: Address,
    bo: Address,
    cy: Address,
    payee: Address,
}

fn member(address: &Address, weight: u32) -> Member {
    Member {
        address: address.clone(),
        weight,
    }
}

/// Three members of equal weight, one recipient they may pay, and a million of
/// a token in the treasury.
fn setup(policy: Policy) -> Fixture<'static> {
    let env = Env::default();
    env.ledger().set_timestamp(START);

    let ada = Address::generate(&env);
    let bo = Address::generate(&env);
    let cy = Address::generate(&env);
    let payee = Address::generate(&env);

    let contract = env.register(
        DaoTreasury,
        (
            policy,
            vec![&env, member(&ada, 1), member(&bo, 1), member(&cy, 1)],
            vec![&env, payee.clone()],
        ),
    );
    let client = DaoTreasuryClient::new(&env, &contract);

    let issuer = Address::generate(&env);
    let asset = env.register_stellar_asset_contract_v2(issuer);
    let token = asset.address();
    env.mock_all_auths();
    token::StellarAssetClient::new(&env, &token).mint(&contract, &1_000_000);
    // Nothing is left granted. Each test says what it grants.
    env.set_auths(&[]);

    Fixture {
        env,
        client,
        contract,
        token,
        ada,
        bo,
        cy,
        payee,
    }
}

fn memo(env: &Env) -> BytesN<32> {
    BytesN::from_array(env, &[7u8; 32])
}

impl Fixture<'_> {
    fn pay(&self, amount: i128) -> Action {
        Action::Pay(Payment {
            token: self.token.clone(),
            to: self.payee.clone(),
            amount,
        })
    }

    fn balance(&self, of: &Address) -> i128 {
        token::Client::new(&self.env, &self.token).balance(of)
    }

    fn at(&self, seconds_after_start: u64) {
        self.env.ledger().set_timestamp(START + seconds_after_start);
    }

    /// Opens a proposal and has Ada and Bo vote for it: two of three, which
    /// under `strict` settles it at once.
    fn passed(&self, action: &Action) -> u32 {
        self.env.mock_all_auths();
        let id = self.client.propose(&self.ada, action, &memo(&self.env));
        self.client.vote(&self.ada, &id, &true);
        self.client.vote(&self.bo, &id, &true);
        id
    }
}

fn refused<T: core::fmt::Debug, E: core::fmt::Debug>(
    result: Result<Result<T, E>, Result<Error, soroban_sdk::InvokeError>>,
) -> Error {
    result
        .err()
        .expect("was not refused")
        .expect("refused, but not by the contract")
}

// ---------------------------------------------------------------------------
// How a treasury starts
// ---------------------------------------------------------------------------

#[test]
fn a_treasury_starts_with_its_rules_its_members_and_whom_it_may_pay() {
    let f = setup(strict());

    assert_eq!(f.client.version(), 2);
    assert_eq!(f.client.policy(), strict());
    assert_eq!(
        f.client.members(),
        vec![&f.env, f.ada.clone(), f.bo.clone(), f.cy.clone()]
    );
    assert_eq!(f.client.total_weight(), 3);
    assert_eq!(f.client.member_weight(&f.bo), 1);
    assert_eq!(f.client.member_weight(&f.payee), 0);
    assert_eq!(f.client.allowlist(), vec![&f.env, f.payee.clone()]);
    assert_eq!(f.client.proposal_count(), 0);
}

/// Creating one fails outright rather than leaving something half set up.
fn creation_fails(policy: Policy, build: fn(&Env) -> Vec<Member>) -> bool {
    std::panic::catch_unwind(|| {
        let env = Env::default();
        let members = build(&env);
        env.register(DaoTreasury, (policy, members, Vec::<Address>::new(&env)));
    })
    .is_err()
}

fn three(env: &Env) -> Vec<Member> {
    vec![
        env,
        member(&Address::generate(env), 1),
        member(&Address::generate(env), 1),
        member(&Address::generate(env), 1),
    ]
}

#[test]
fn a_treasury_that_could_not_work_cannot_be_created() {
    let policies = [
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
        // Voting that is open for no time at all.
        Policy {
            voting_period: 0,
            ..strict()
        },
        Policy {
            spend_limit: -1,
            ..strict()
        },
        // A limit with no window never resets.
        Policy {
            spend_limit: 100,
            spend_window: 0,
            ..strict()
        },
        // Periods so long that adding one to the time would overflow, and
        // every proposal after it would fail — the one to put it right too.
        Policy {
            voting_period: u64::MAX,
            ..strict()
        },
        Policy {
            timelock: u64::MAX,
            ..strict()
        },
        Policy {
            spend_limit: 100,
            spend_window: u64::MAX,
            ..strict()
        },
        Policy {
            voting_period: 366 * DAY,
            ..strict()
        },
    ];
    for policy in policies {
        assert!(
            creation_fails(policy.clone(), three),
            "created with {policy:?}"
        );
    }

    // And the members have to be members.
    assert!(
        creation_fails(strict(), |env| Vec::new(env)),
        "created with nobody in it"
    );
    assert!(
        creation_fails(strict(), |env| vec![
            env,
            member(&Address::generate(env), 0)
        ]),
        "created with a member who has no say"
    );
    assert!(
        creation_fails(strict(), |env| {
            let twice = Address::generate(env);
            vec![env, member(&twice, 1), member(&twice, 2)]
        }),
        "created with one member listed twice"
    );

    // The premise: the same code does create one when it is given something
    // that can work.
    assert!(!creation_fails(strict(), three));
}

// ---------------------------------------------------------------------------
// Nobody stands above the members
// ---------------------------------------------------------------------------

/// The claim this version exists to make, tested by handing over every key.
///
/// Every `require_auth` passes here, for anybody. One member, with all of that,
/// tries each thing that would give them the treasury: a payment to themselves,
/// the policy, their own weight, their own place on the list of recipients.
/// Each is a proposal and nothing else, and one vote of three settles none of
/// them. The version before this one let an admin key do all four directly.
#[test]
fn no_single_member_can_move_the_money_or_change_the_rules() {
    let f = setup(strict());
    f.env.mock_all_auths();

    let grabs = [
        // Pay themselves. Not on the list of recipients, so refused outright.
        Action::Pay(Payment {
            token: f.token.clone(),
            to: f.ada.clone(),
            amount: 1_000_000,
        }),
    ];
    for action in grabs {
        let err = refused(f.client.try_propose(&f.ada, &action, &memo(&f.env)));
        assert_eq!(err, Error::RecipientNotAllowed);
    }

    let lone = [
        f.pay(1_000_000),
        Action::SetPolicy(Policy {
            quorum_bps: 1,
            approval_bps: 5_001,
            voting_period: 1,
            timelock: 0,
            spend_limit: 1_000_000,
            spend_window: 1,
        }),
        Action::SetMember(member(&f.ada, 1_000)),
        Action::AllowRecipient(f.ada.clone()),
    ];
    for action in lone {
        let id = f.client.propose(&f.ada, &action, &memo(&f.env));
        f.client.vote(&f.ada, &id, &true);

        // One of three is not settled, and cannot be carried out — now, or
        // once voting has closed, when it is rejected for want of a quorum.
        assert_eq!(f.client.proposal(&id).status, Status::Open);
        assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);

        f.at(3 * DAY);
        assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);
        assert_eq!(f.client.tally(&id), Status::Rejected);
        f.at(0);
    }

    // Nothing moved and nothing changed.
    assert_eq!(f.balance(&f.contract), 1_000_000);
    assert_eq!(f.balance(&f.ada), 0);
    assert_eq!(f.client.policy(), strict());
    assert_eq!(f.client.member_weight(&f.ada), 1);
    assert_eq!(f.client.allowlist(), vec![&f.env, f.payee.clone()]);
    // And the allowance for spending alone is what it was set to: nothing.
    assert_eq!(
        refused(f.client.try_spend(&f.ada, &f.token, &f.payee, &1)),
        Error::SpendLimitExceeded
    );
}

/// A rule is changed the way a payment is made.
#[test]
fn the_rules_change_by_proposal_and_only_once_it_has_passed_and_waited() {
    let f = setup(strict());
    let dee = Address::generate(&f.env);
    let supplier = Address::generate(&f.env);
    let looser = Policy {
        timelock: 2 * DAY,
        spend_limit: 500,
        spend_window: DAY,
        ..strict()
    };

    let changes = [
        Action::SetPolicy(looser.clone()),
        Action::SetMember(member(&dee, 2)),
        Action::AllowRecipient(supplier.clone()),
    ];
    for action in changes {
        let id = f.passed(&action);
        // Passed is not done. The timelock runs first.
        assert_eq!(f.client.proposal(&id).status, Status::Passed);
        assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);
        assert_eq!(f.client.policy(), strict());
        assert_eq!(f.client.member_weight(&dee), 0);
        assert_eq!(f.client.allowlist().len(), 1);
    }

    f.at(DAY);
    f.env.set_auths(&[]);
    for id in 1..=3u32 {
        f.client.execute(&id);
    }
    assert_eq!(f.client.policy(), looser);
    assert_eq!(f.client.member_weight(&dee), 2);
    assert_eq!(f.client.total_weight(), 5);
    assert_eq!(
        f.client.allowlist(),
        vec![&f.env, f.payee.clone(), supplier]
    );
}

/// The one way to make a treasury permanently unable to act, refused when it is
/// proposed rather than after a week of voting on it.
#[test]
fn a_change_that_would_strand_the_treasury_is_refused() {
    let env = Env::default();
    let only = Address::generate(&env);
    let contract = env.register(
        DaoTreasury,
        (
            strict(),
            vec![&env, member(&only, 1)],
            Vec::<Address>::new(&env),
        ),
    );
    let client = DaoTreasuryClient::new(&env, &contract);
    env.mock_all_auths();

    let err = refused(client.try_propose(&only, &Action::SetMember(member(&only, 0)), &memo(&env)));
    assert_eq!(err, Error::WouldLockOut);

    // A policy nobody could ever pass anything under is not one to vote on.
    let err = refused(client.try_propose(
        &only,
        &Action::SetPolicy(Policy {
            approval_bps: 5_000,
            ..strict()
        }),
        &memo(&env),
    ));
    assert_eq!(err, Error::InvalidPolicy);
}

// ---------------------------------------------------------------------------
// Proposing and voting
// ---------------------------------------------------------------------------

#[test]
fn only_a_member_proposes() {
    let f = setup(strict());
    let stranger = Address::generate(&f.env);
    f.env.mock_all_auths();

    let err = refused(f.client.try_propose(&stranger, &f.pay(100), &memo(&f.env)));
    assert_eq!(err, Error::NotMember);
}

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
            args: (f.bo.clone(), f.pay(100), memo(&f.env)).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    let attempt = f.client.try_propose(&f.bo, &f.pay(100), &memo(&f.env));
    assert!(attempt.is_err(), "one member proposed in another's name");
    assert_eq!(f.client.proposal_count(), 0);
}

#[test]
fn a_payment_that_could_not_be_made_is_refused_when_it_is_proposed() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let unlisted = Address::generate(&f.env);

    let to_stranger = Action::Pay(Payment {
        token: f.token.clone(),
        to: unlisted,
        amount: 100,
    });
    assert_eq!(
        refused(f.client.try_propose(&f.ada, &to_stranger, &memo(&f.env))),
        Error::RecipientNotAllowed
    );
    for amount in [0i128, -5] {
        assert_eq!(
            refused(f.client.try_propose(&f.ada, &f.pay(amount), &memo(&f.env))),
            Error::InvalidAmount
        );
    }
}

#[test]
fn a_member_votes_once() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));

    assert_eq!(f.client.voted(&id, &f.ada), None);
    f.client.vote(&f.ada, &id, &false);
    assert_eq!(f.client.voted(&id, &f.ada), Some(false));

    assert_eq!(
        refused(f.client.try_vote(&f.ada, &id, &true)),
        Error::AlreadyVoted
    );
    let p = f.client.proposal(&id);
    assert_eq!((p.yes, p.no), (0, 1));
}

/// Without blanket mocking: a vote in Bo's name, signed by Ada.
#[test]
fn a_vote_needs_the_voters_signature() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));

    f.env.mock_auths(&[MockAuth {
        address: &f.ada,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "vote",
            args: (f.bo.clone(), id, true).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    assert!(
        f.client.try_vote(&f.bo, &id, &true).is_err(),
        "one member voted as another"
    );
    assert_eq!(f.client.proposal(&id).yes, 0);
}

#[test]
fn a_stranger_does_not_vote() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));
    let stranger = Address::generate(&f.env);

    assert_eq!(
        refused(f.client.try_vote(&stranger, &id, &true)),
        Error::NotMember
    );
}

#[test]
fn voting_closes() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));

    f.at(DAY);
    assert_eq!(
        refused(f.client.try_vote(&f.bo, &id, &true)),
        Error::NotOpen
    );
}

/// Quorum is how many turned up; approval is how many of those agreed.
#[test]
fn quorum_and_approval_are_two_different_questions() {
    // Five members, so that a proposal can have a quorum and not a majority,
    // or a majority of too few.
    let env = Env::default();
    env.ledger().set_timestamp(START);
    let people: std::vec::Vec<Address> = (0..5).map(|_| Address::generate(&env)).collect();
    let mut members = Vec::new(&env);
    for p in &people {
        members.push_back(member(p, 1));
    }
    let payee = Address::generate(&env);
    let contract = env.register(DaoTreasury, (strict(), members, vec![&env, payee.clone()]));
    let client = DaoTreasuryClient::new(&env, &contract);
    let token = env
        .register_stellar_asset_contract_v2(Address::generate(&env))
        .address();
    env.mock_all_auths();
    let pay = Action::Pay(Payment {
        token,
        to: payee,
        amount: 1,
    });

    // One for, none against: unanimous, and nowhere near a quorum of five.
    let few = client.propose(&people[0], &pay, &memo(&env));
    client.vote(&people[0], &few, &true);

    // Two for, two against: a quorum, and not sixty per cent of it.
    let split = client.propose(&people[0], &pay, &memo(&env));
    client.vote(&people[0], &split, &true);
    client.vote(&people[1], &split, &true);
    client.vote(&people[2], &split, &false);
    client.vote(&people[3], &split, &false);

    // Two for, one against: a quorum of three, and two thirds of it.
    let enough = client.propose(&people[0], &pay, &memo(&env));
    client.vote(&people[0], &enough, &true);
    client.vote(&people[1], &enough, &true);
    client.vote(&people[2], &enough, &false);

    // None of them is settled while the votes not yet cast could change it.
    for id in [few, split, enough] {
        assert_eq!(client.proposal(&id).status, Status::Open, "proposal {id}");
    }

    env.ledger().set_timestamp(START + DAY);
    assert_eq!(client.tally(&few), Status::Rejected);
    assert_eq!(client.tally(&split), Status::Rejected);
    assert_eq!(client.tally(&enough), Status::Passed);
}

// ---------------------------------------------------------------------------
// Settling early
// ---------------------------------------------------------------------------

/// Two of three in favour cannot be outvoted by the third. Waiting for the
/// voting window to close would be waiting for a vote that cannot matter.
#[test]
fn an_outcome_that_cannot_change_is_settled_at_once() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));

    f.client.vote(&f.ada, &id, &true);
    assert_eq!(
        f.client.proposal(&id).status,
        Status::Open,
        "one of three settled it"
    );

    f.at(3_600);
    f.client.vote(&f.bo, &id, &true);
    let p = f.client.proposal(&id);
    assert_eq!(p.status, Status::Passed);
    // The timelock runs in full from the moment it was settled, an hour in.
    assert_eq!(p.executable_at, START + 3_600 + DAY);
    // And the third member's vote is no longer taken.
    assert_eq!(
        refused(f.client.try_vote(&f.cy, &id, &false)),
        Error::NotOpen
    );
}

#[test]
fn a_proposal_that_can_no_longer_pass_is_closed_at_once() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));

    f.client.vote(&f.ada, &id, &false);
    assert_eq!(f.client.proposal(&id).status, Status::Open);
    f.client.vote(&f.bo, &id, &false);
    assert_eq!(f.client.proposal(&id).status, Status::Rejected);
    assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);
}

/// Where a quorum asks for more than approval does, staying away is still a way
/// to stop a proposal. Two of three in favour is enough agreement here, and not
/// yet enough turnout — so it is not settled on agreement alone.
#[test]
fn enough_in_favour_is_not_settled_before_enough_have_voted() {
    let f = setup(Policy {
        quorum_bps: 10_000,
        approval_bps: 6_000,
        ..strict()
    });
    f.env.mock_all_auths();

    let left = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));
    let joined = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));
    for id in [left, joined] {
        f.client.vote(&f.ada, &id, &true);
        f.client.vote(&f.bo, &id, &true);
        assert_eq!(f.client.proposal(&id).status, Status::Open, "proposal {id}");
        assert_eq!(refused(f.client.try_tally(&id)), Error::Undecided);
    }

    // The third turns up to one, votes against, and by doing so makes the
    // quorum that lets it pass.
    f.client.vote(&f.cy, &joined, &false);
    assert_eq!(f.client.proposal(&joined).status, Status::Passed);

    // And stays away from the other, which fails for want of one.
    f.at(DAY);
    assert_eq!(f.client.tally(&left), Status::Rejected);
}

/// Settling early never shortens the wait. That wait is the window in which the
/// rest of a DAO notices that something which passed should not have.
#[test]
fn settling_early_does_not_shorten_the_timelock() {
    let f = setup(strict());
    let id = f.passed(&f.pay(100));

    // Settled at the start. A full timelock from then, to the second.
    f.at(DAY - 1);
    assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);
    assert_eq!(f.balance(&f.payee), 0);

    f.at(DAY);
    f.client.execute(&id);
    assert_eq!(f.balance(&f.payee), 100);
}

/// A proposal nobody settles early still becomes executable when it would have:
/// at the end of voting plus the timelock, with no call to tally in between.
#[test]
fn a_proposal_that_passes_at_the_close_can_be_executed_after_its_timelock() {
    // Quorum of two thirds and approval of all who voted, so that two votes
    // of three in favour is enough to pass at the close and not enough to
    // settle it early.
    let f = setup(Policy {
        quorum_bps: 6_000,
        approval_bps: 10_000,
        ..strict()
    });
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(250), &memo(&f.env));
    f.client.vote(&f.ada, &id, &true);
    f.client.vote(&f.bo, &id, &true);
    assert_eq!(f.client.proposal(&id).status, Status::Open);

    f.at(2 * DAY - 1);
    assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);

    f.at(2 * DAY);
    f.client.execute(&id);
    assert_eq!(f.client.proposal(&id).status, Status::Executed);
    assert_eq!(f.balance(&f.payee), 250);
    assert_eq!(f.balance(&f.contract), 999_750);
}

// ---------------------------------------------------------------------------
// Carrying out
// ---------------------------------------------------------------------------

/// The claim the module comment makes, tested with no auth mocked at all so it
/// cannot pass because of a fixture. A contract whose funds need a specific
/// person online is a contract with a hostage — and stelfin's uptime must never
/// be a condition of a DAO spending its own money.
#[test]
fn execute_needs_nobodys_signature() {
    let f = setup(strict());
    let id = f.passed(&f.pay(100));
    f.at(DAY);

    // No signatures available at all. Anyone at all can carry this out.
    f.env.set_auths(&[]);
    f.client.execute(&id);

    assert_eq!(f.client.proposal(&id).status, Status::Executed);
    assert_eq!(f.balance(&f.payee), 100);
}

#[test]
fn a_proposal_executes_once() {
    let f = setup(strict());
    let id = f.passed(&f.pay(100));
    f.at(DAY);

    f.client.execute(&id);
    assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);
    assert_eq!(f.balance(&f.payee), 100, "it paid twice");
}

/// What the timelock is for. A payment has passed and is waiting; the members
/// decide the recipient should not be paid after all. Taking them off the list
/// needs the votes and does not wait out a timelock of its own — if it did, it
/// could never land before the payment it was meant to stop.
#[test]
fn a_recipient_removed_during_the_timelock_is_not_paid() {
    let f = setup(strict());
    let payment = f.passed(&f.pay(100));

    f.at(3_600);
    let removal = f.passed(&Action::DenyRecipient(f.payee.clone()));
    // Carried out the moment it is decided, an hour into a day's timelock.
    f.client.execute(&removal);
    assert_eq!(f.client.allowlist().len(), 0);

    f.at(DAY);
    assert_eq!(
        refused(f.client.try_execute(&payment)),
        Error::RecipientNotAllowed
    );
    assert_eq!(f.balance(&f.payee), 0);
    // It was not marked done by an attempt that did nothing.
    assert_eq!(f.client.proposal(&payment).status, Status::Passed);
}

/// The other thing it is for, and the one that works for a change to the rules
/// as well as for a payment: the members stop the proposal itself.
#[test]
fn the_members_can_stop_a_proposal_that_has_passed() {
    let f = setup(strict());
    let dee = Address::generate(&f.env);
    let change = f.passed(&Action::SetMember(member(&dee, 100)));

    // An hour into the timelock, Bo has thought again and Cy never agreed.
    f.at(3_600);
    let stop = f
        .client
        .propose(&f.bo, &Action::Revoke(change), &memo(&f.env));
    f.client.vote(&f.bo, &stop, &true);
    // One member does not stop what the members passed.
    assert_eq!(refused(f.client.try_execute(&stop)), Error::NotExecutable);
    assert_eq!(f.client.proposal(&change).status, Status::Passed);

    f.client.vote(&f.cy, &stop, &true);
    // No timelock of its own, and nobody's signature.
    f.env.set_auths(&[]);
    f.client.execute(&stop);
    assert_eq!(f.client.proposal(&change).status, Status::Cancelled);

    f.at(2 * DAY);
    assert_eq!(refused(f.client.try_execute(&change)), Error::NotExecutable);
    assert_eq!(f.client.member_weight(&dee), 0);
    assert_eq!(f.client.total_weight(), 3);
}

#[test]
fn there_is_nothing_to_stop_once_a_proposal_has_been_carried_out() {
    let f = setup(strict());
    let payment = f.passed(&f.pay(100));
    let stop = f.passed(&Action::Revoke(payment));

    // Nobody carried the stop out, and the payment's timelock ran.
    f.at(DAY);
    f.client.execute(&payment);
    assert_eq!(refused(f.client.try_execute(&stop)), Error::NothingToRevoke);
    assert_eq!(f.client.proposal(&payment).status, Status::Executed);
    assert_eq!(f.balance(&f.payee), 100);

    // Nor is there anything to propose against, or against one that never was.
    f.env.mock_all_auths();
    assert_eq!(
        refused(
            f.client
                .try_propose(&f.ada, &Action::Revoke(payment), &memo(&f.env))
        ),
        Error::NothingToRevoke
    );
    assert_eq!(
        refused(
            f.client
                .try_propose(&f.ada, &Action::Revoke(99), &memo(&f.env))
        ),
        Error::UnknownProposal
    );
}

/// Removing a recipient still takes the votes. One member cannot do it.
#[test]
fn removing_a_recipient_still_needs_the_votes() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(
        &f.ada,
        &Action::DenyRecipient(f.payee.clone()),
        &memo(&f.env),
    );
    f.client.vote(&f.ada, &id, &true);

    assert_eq!(refused(f.client.try_execute(&id)), Error::NotExecutable);
    assert_eq!(f.client.allowlist().len(), 1);
}

/// Whoever made a proposal may withdraw it, and nobody else.
#[test]
fn only_its_proposer_withdraws_a_proposal() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));

    // Another member, with every signature granted: it is not theirs.
    assert_eq!(refused(f.client.try_cancel(&f.bo, &id)), Error::NotProposer);

    // In the proposer's name, signed by somebody else.
    f.env.mock_auths(&[MockAuth {
        address: &f.bo,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "cancel",
            args: (f.ada.clone(), id).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    assert!(
        f.client.try_cancel(&f.ada, &id).is_err(),
        "withdrawn without its proposer"
    );
    assert_eq!(f.client.proposal(&id).status, Status::Open);

    f.env.mock_all_auths();
    f.client.cancel(&f.ada, &id);
    assert_eq!(f.client.proposal(&id).status, Status::Cancelled);
    assert_eq!(
        refused(f.client.try_vote(&f.bo, &id, &true)),
        Error::NotOpen
    );
    assert_eq!(refused(f.client.try_cancel(&f.ada, &id)), Error::NotOpen);
}

// ---------------------------------------------------------------------------
// Spending without a proposal
// ---------------------------------------------------------------------------

#[test]
fn spending_alone_is_off_until_the_members_turn_it_on() {
    let f = setup(strict());
    f.env.mock_all_auths();

    assert_eq!(f.client.spendable(), 0);
    assert_eq!(
        refused(f.client.try_spend(&f.ada, &f.token, &f.payee, &1)),
        Error::SpendLimitExceeded
    );

    // By vote, like any other change to the policy.
    let id = f.passed(&Action::SetPolicy(Policy {
        spend_limit: 500,
        spend_window: DAY,
        ..strict()
    }));
    f.at(DAY);
    f.client.execute(&id);

    f.client.spend(&f.ada, &f.token, &f.payee, &200);
    assert_eq!(f.balance(&f.payee), 200);
    assert_eq!(f.client.spendable(), 300);
}

/// The one thing a member can do alone, and what bounds it: an amount the
/// members set, to somebody the members allowed.
#[test]
fn what_one_member_can_spend_is_bounded_by_what_was_voted() {
    let f = setup(Policy {
        spend_limit: 500,
        spend_window: DAY,
        ..strict()
    });
    f.env.mock_all_auths();
    let unlisted = Address::generate(&f.env);

    f.client.spend(&f.ada, &f.token, &f.payee, &300);
    assert_eq!(
        refused(f.client.try_spend(&f.ada, &f.token, &f.payee, &201)),
        Error::SpendLimitExceeded
    );
    // Another member draws on the same window, not one of their own.
    assert_eq!(
        refused(f.client.try_spend(&f.bo, &f.token, &f.payee, &201)),
        Error::SpendLimitExceeded
    );
    assert_eq!(
        refused(f.client.try_spend(&f.ada, &f.token, &unlisted, &1)),
        Error::RecipientNotAllowed
    );
    assert_eq!(
        refused(f.client.try_spend(&f.ada, &f.token, &f.ada, &1)),
        Error::RecipientNotAllowed
    );
    for amount in [0i128, -1] {
        assert_eq!(
            refused(f.client.try_spend(&f.ada, &f.token, &f.payee, &amount)),
            Error::InvalidAmount
        );
    }
    assert_eq!(f.balance(&f.payee), 300);
}

#[test]
fn the_spend_window_rolls_over() {
    let f = setup(Policy {
        spend_limit: 500,
        spend_window: DAY,
        ..strict()
    });
    f.env.mock_all_auths();

    f.client.spend(&f.ada, &f.token, &f.payee, &500);
    assert_eq!(f.client.spendable(), 0);

    f.at(DAY - 1);
    assert_eq!(
        refused(f.client.try_spend(&f.ada, &f.token, &f.payee, &1)),
        Error::SpendLimitExceeded
    );

    f.at(DAY);
    assert_eq!(f.client.spendable(), 500);
    f.client.spend(&f.bo, &f.token, &f.payee, &500);
    assert_eq!(f.balance(&f.payee), 1_000);
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
    assert_eq!(f.balance(&f.payee), 0);

    let stranger = Address::generate(&f.env);
    f.env.mock_all_auths();
    assert_eq!(
        refused(f.client.try_spend(&stranger, &f.token, &f.payee, &100)),
        Error::NotMember
    );
}

// ---------------------------------------------------------------------------
// What it says happened, and staying alive
// ---------------------------------------------------------------------------

/// A treasury's history is readable from the ledger: each change of state says
/// so, and only this contract's own events are counted here.
#[test]
fn every_change_of_state_is_published() {
    let f = setup(strict());
    f.env.mock_all_auths();
    let ours = |f: &Fixture| {
        f.env
            .events()
            .all()
            .filter_by_contract(&f.contract)
            .events()
            .len()
    };

    let id = f.client.propose(&f.ada, &f.pay(100), &memo(&f.env));
    assert_eq!(ours(&f), 1, "proposing");

    f.client.vote(&f.ada, &id, &true);
    assert_eq!(ours(&f), 1, "a vote that settles nothing");

    f.client.vote(&f.bo, &id, &true);
    assert_eq!(ours(&f), 2, "the vote, and the decision it made");

    f.at(DAY);
    f.client.execute(&id);
    assert_eq!(ours(&f), 1, "carrying it out");
}

/// Anyone can keep a treasury alive, and it changes nothing else: its rules, who
/// its members are and whom it may pay all last as long as each other.
#[test]
fn anyone_can_keep_a_treasury_alive() {
    let f = setup(strict());
    f.env.set_auths(&[]);
    let lifetimes = |f: &Fixture| {
        f.env.as_contract(&f.contract, || {
            [
                f.env.storage().instance().get_ttl(),
                f.env
                    .storage()
                    .persistent()
                    .get_ttl(&Key::Member(f.cy.clone())),
                f.env
                    .storage()
                    .persistent()
                    .get_ttl(&Key::Allowed(f.payee.clone())),
            ]
        })
    };
    // The premise: a member who has never taken part has a place that would
    // lapse long before the treasury itself.
    assert!(lifetimes(&f)[1] < KEEP_LEDGERS / 2);

    f.client.bump();
    assert_eq!(lifetimes(&f), [KEEP_LEDGERS; 3]);
    assert_eq!(f.client.policy(), strict());
    assert_eq!(f.client.total_weight(), 3);
}

// ---------------------------------------------------------------------------
// How it looks from outside
// ---------------------------------------------------------------------------

/// The exact bytes this contract's values travel as.
///
/// Three programs have to agree about them: this one, the server that builds
/// the calls, and the page a member signs on, which reads a call back out of
/// the envelope and says in words what it does. Each checks itself against the
/// same file, so a field renamed or reordered here turns two other suites red
/// instead of turning a signing page into one that misreads what it shows.
///
/// `UPDATE_GOLDEN=1 cargo test` rewrites the file. Doing that is a statement
/// that the other two have been changed to match.
#[test]
fn its_values_are_encoded_the_way_the_golden_file_says() {
    use soroban_sdk::xdr::ToXdr;
    use soroban_sdk::{IntoVal, Val};
    use std::string::String;

    let env = Env::default();
    let ada = Address::from_str(
        &env,
        "GDKQYYPKNOHKK6W5JVT57WZDEWHQY5IQYLZHFNXS2326H7ROC5ZU5763",
    );
    let payee = Address::from_str(
        &env,
        "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5",
    );
    let token = Address::from_str(
        &env,
        "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA",
    );

    let policy = Policy {
        quorum_bps: 5_000,
        approval_bps: 6_000,
        voting_period: 259_200,
        timelock: 86_400,
        spend_limit: 2_500_000_000,
        spend_window: 86_400,
    };
    let pay = Action::Pay(Payment {
        token: token.clone(),
        to: payee.clone(),
        amount: 12_500_000_000,
    });
    let proposal = crate::types::Proposal {
        id: 3,
        proposer: ada.clone(),
        action: pay.clone(),
        memo: memo(&env),
        status: Status::Passed,
        closes_at: 1_700_086_400,
        executable_at: 1_700_090_000,
        yes: 2,
        no: 1,
    };

    let hex = |value: Val| -> String {
        let mut out = String::new();
        for byte in value.to_xdr(&env).iter() {
            out.push_str(&std::format!("{byte:02x}"));
        }
        out
    };
    let values: [(&str, Val); 13] = [
        ("policy", policy.clone().into_val(&env)),
        ("member", member(&ada, 2).into_val(&env)),
        (
            "members",
            vec![&env, member(&ada, 2), member(&payee, 1)].into_val(&env),
        ),
        (
            "addresses",
            vec![&env, payee.clone(), ada.clone()].into_val(&env),
        ),
        ("action_pay", pay.into_val(&env)),
        (
            "action_set_policy",
            Action::SetPolicy(policy).into_val(&env),
        ),
        (
            "action_set_member",
            Action::SetMember(member(&payee, 0)).into_val(&env),
        ),
        (
            "action_allow_recipient",
            Action::AllowRecipient(payee.clone()).into_val(&env),
        ),
        (
            "action_deny_recipient",
            Action::DenyRecipient(payee).into_val(&env),
        ),
        ("action_revoke", Action::Revoke(7).into_val(&env)),
        ("status_passed", Status::Passed.into_val(&env)),
        ("memo", memo(&env).into_val(&env)),
        ("proposal", proposal.into_val(&env)),
    ];

    let mut ours = String::new();
    for (name, value) in values {
        ours.push_str(name);
        ours.push('\t');
        ours.push_str(&hex(value));
        ours.push('\n');
    }

    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("testdata/abi.tsv");
    if std::env::var("UPDATE_GOLDEN").is_ok() {
        std::fs::write(&path, &ours).expect("write the golden file");
    }
    let golden = std::fs::read_to_string(&path).expect("read the golden file");
    assert_eq!(
        ours, golden,
        "the encoding has changed: see the comment on this test"
    );
}
