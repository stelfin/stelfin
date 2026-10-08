#![no_std]

//! A DAO's treasury policy and its proposals, in one contract.
//!
//! Merged rather than split, and that is a decision worth stating. Treasury
//! policy and voting are one state machine over one pot of money: splitting
//! them buys a cross-contract authorisation dance and a window in which the two
//! disagree about whether a proposal passed. There is no version of that
//! disagreement that is not a bug about money.
//!
//! # Nobody stands above the members
//!
//! There is no admin. Everything that moves money or changes who decides — a
//! payment, the policy, who is a member and with what weight, who may be paid —
//! is a proposal, and a proposal does nothing until the members have passed it
//! and its timelock has run.
//!
//! The timelock is the members' chance to stop something that passed and
//! should not have, and stopping it is a proposal too: one that takes a
//! recipient off the list, or revokes the proposal outright. Those two only
//! ever make the treasury able to do less, so they do not wait out a timelock
//! of their own. They need the votes like anything else.
//!
//! The version before this one had an admin whose key could replace the policy,
//! add itself as a member, allow itself as a recipient and spend: the whole
//! treasury, in four calls, with no vote. A treasury like that is held by one
//! key and decorated with a voting system. This one is held by its members.
//!
//! The one thing a single member can do alone is spend under the rolling limit,
//! to a recipient already allowed — and both that limit and that list are set
//! by vote, and both start at nothing.
//!
//! # What this contract does not claim
//!
//! It enforces policy only over funds it holds. A DAO whose treasury is a
//! classic M-of-N account can point this contract at nothing — any M signers
//! move that money directly and this contract never hears about it.
//!
//! # Why `execute` takes no authorisation
//!
//! Once a proposal has passed and its timelock has run, anyone may execute it.
//! Requiring a particular signer would make somebody's availability a condition
//! of the treasury's — and the whole point of a timelock is that the decision
//! is already made and merely waiting. A contract whose funds need a specific
//! person online is a contract with a hostage.

mod events;
mod policy;
mod proposals;
mod spend;
mod types;

#[cfg(test)]
mod test;

use soroban_sdk::{
    contract, contractimpl, contractmeta, panic_with_error, Address, BytesN, Env, Vec,
};

pub use types::{Action, Error, Member, Payment, Policy, Proposal, Status};

contractmeta!(
    key = "Description",
    val = "Treasury policy and proposals for a DAO, from its chat platform."
);

/// Which shape of this contract an instance is. Two: the one with no admin.
const VERSION: u32 = 2;

/// Storage keys.
///
/// An enum rather than strings, so a typo is a compile error rather than a
/// silently empty read — which for a member weight would read as "not a
/// member" and for a policy as "no limits at all".
#[soroban_sdk::contracttype]
#[derive(Clone)]
pub(crate) enum Key {
    Policy,
    /// Member weight, by address.
    Member(Address),
    /// Every member, so quorum can be computed without an index.
    Members,
    /// Whether a recipient may be paid at all.
    Allowed(Address),
    /// The recipients on the allowlist, for reading back.
    Allowlist,
    NextId,
    Proposal(u32),
    /// One member's vote on one proposal.
    Voted(u32, Address),
    /// Spending done inside the current rolling window.
    Window,
}

#[contract]
pub struct DaoTreasury;

#[contractimpl]
impl DaoTreasury {
    /// Sets the treasury up, in the transaction that creates it.
    ///
    /// A constructor rather than a call made afterwards, for two reasons. A
    /// contract that is created in one transaction and set up in another is,
    /// in between, a contract anybody can set up. And everything a treasury
    /// starts with — its rules, its members, whom it may pay — is then one
    /// thing, signed once, by whoever created it, and readable from the
    /// transaction that did.
    ///
    /// From here on none of it changes except by proposal.
    pub fn __constructor(env: Env, policy: Policy, members: Vec<Member>, allowlist: Vec<Address>) {
        if let Err(err) = policy::setup(&env, &policy, &members, &allowlist) {
            panic_with_error!(&env, err);
        }
        policy::keep_alive(&env);
    }

    /// Which shape of this contract this is.
    pub fn version(_env: Env) -> u32 {
        VERSION
    }

    /// Reports the policy in force.
    pub fn policy(env: Env) -> Result<Policy, Error> {
        policy::load(&env)
    }

    /// Reports one member's weight. Zero for anyone who is not one.
    pub fn member_weight(env: Env, member: Address) -> u32 {
        policy::weight_of(&env, &member)
    }

    /// Reports every member.
    pub fn members(env: Env) -> Vec<Address> {
        policy::members(&env)
    }

    /// Reports the total weight a proposal is measured against.
    pub fn total_weight(env: Env) -> u32 {
        policy::total_weight(&env)
    }

    /// Reports the recipients this treasury may pay.
    pub fn allowlist(env: Env) -> Vec<Address> {
        policy::allowlist(&env)
    }

    /// Puts something to the members: a payment, or a change to the rules.
    pub fn propose(
        env: Env,
        proposer: Address,
        action: Action,
        memo: BytesN<32>,
    ) -> Result<u32, Error> {
        policy::keep_alive(&env);
        proposals::propose(&env, &proposer, &action, &memo)
    }

    /// Records one member's vote, and settles the proposal if that settles it.
    pub fn vote(env: Env, voter: Address, id: u32, approve: bool) -> Result<(), Error> {
        policy::keep_alive(&env);
        proposals::vote(&env, &voter, id, approve)
    }

    /// Reports how a member voted on a proposal, if they have.
    pub fn voted(env: Env, id: u32, voter: Address) -> Option<bool> {
        proposals::voted(&env, id, &voter)
    }

    /// Reports a proposal.
    pub fn proposal(env: Env, id: u32) -> Result<Proposal, Error> {
        proposals::load(&env, id)
    }

    /// Reports how many proposals there have been. They are numbered from one.
    pub fn proposal_count(env: Env) -> u32 {
        let next: u32 = env.storage().instance().get(&Key::NextId).unwrap_or(1);
        next - 1
    }

    /// Settles a proposal whose outcome is no longer in question, and records
    /// it.
    ///
    /// Separate from `execute` because a proposal that fails still has to stop
    /// being open, and nobody has an incentive to execute one that lost. Anyone
    /// may call it, for the same reason anyone may execute.
    pub fn tally(env: Env, id: u32) -> Result<Status, Error> {
        policy::keep_alive(&env);
        proposals::tally(&env, id)
    }

    /// Carries out a passed proposal.
    ///
    /// Deliberately takes no authorisation. See the module comment: the
    /// decision was made when the votes were cast, and requiring a particular
    /// person to be online afterwards would make this deployment a liveness
    /// dependency for somebody else's money.
    pub fn execute(env: Env, id: u32) -> Result<(), Error> {
        policy::keep_alive(&env);
        proposals::execute(&env, id)
    }

    /// Withdraws a proposal. Whoever made it, and only while it is open.
    pub fn cancel(env: Env, proposer: Address, id: u32) -> Result<(), Error> {
        policy::keep_alive(&env);
        proposals::cancel(&env, &proposer, id)
    }

    /// Pays out under the rolling limit, without a proposal.
    ///
    /// Off by default: `spend_limit` starts at whatever the members set, and
    /// zero means every payment is a proposal. Turning it up is how a treasury
    /// trades review for speed on small amounts, and it is the treasury's trade
    /// to make — by vote, like any other change to its policy.
    pub fn spend(
        env: Env,
        member: Address,
        token: Address,
        to: Address,
        amount: i128,
    ) -> Result<(), Error> {
        policy::keep_alive(&env);
        spend::spend(&env, &member, &token, &to, amount)
    }

    /// Reports how much of the rolling window is left.
    pub fn spendable(env: Env) -> Result<i128, Error> {
        spend::remaining(&env)
    }

    /// Keeps the treasury's state alive, without doing anything else: its
    /// rules, its members and whom it may pay.
    ///
    /// Anyone may call it. Using the treasury renews it, so this is for one
    /// that is not being used: state that is left alone long enough is archived
    /// by the network and has to be paid to be brought back, and a treasury's
    /// members should not find that out on the day they next want to vote.
    pub fn bump(env: Env) {
        policy::keep_everything(&env);
    }
}
