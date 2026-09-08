#![no_std]

//! A DAO's treasury policy and its proposals, in one contract.
//!
//! Merged rather than split, and that is a decision worth stating. Treasury
//! policy and voting are one state machine over one pot of money: splitting
//! them buys a cross-contract authorisation dance and a window in which the two
//! disagree about whether a proposal passed. There is no version of that
//! disagreement that is not a bug about money.
//!
//! # What this contract does not claim
//!
//! It enforces policy only over funds it holds. A DAO whose treasury is a
//! classic M-of-N account can point this contract at nothing — any M signers
//! move that money directly and this contract never hears about it. In that
//! configuration this is policy *advice*, and stelfin has to say so, because
//! manufactured assurance is worse than none.
//!
//! # Why `execute` takes no authorisation
//!
//! Once a proposal has passed and its timelock has run, anyone may execute it.
//! Requiring a particular signer would make somebody's availability a condition
//! of the treasury's — and the whole point of a timelock is that the decision
//! is already made and merely waiting. A contract whose funds need a specific
//! person online is a contract with a hostage.

mod policy;
mod proposals;
mod spend;
mod types;

#[cfg(test)]
mod test;

use soroban_sdk::{contract, contractimpl, contractmeta, Address, BytesN, Env, Vec};

pub use types::{Error, Policy, Proposal, Status};

contractmeta!(
    key = "Description",
    val = "Treasury policy and proposals for a DAO, from its chat platform."
);

/// Storage keys.
///
/// An enum rather than strings, so a typo is a compile error rather than a
/// silently empty read — which for a member weight would read as "not a
/// member" and for a policy as "no limits at all".
#[soroban_sdk::contracttype]
#[derive(Clone)]
pub(crate) enum Key {
    Admin,
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
    /// Sets the treasury up, once.
    ///
    /// The admin can change policy and membership and nothing else. It cannot
    /// move money, cannot vote on its own, and cannot execute a proposal that
    /// has not passed — the separation is the point, and a treasury whose admin
    /// key is stolen loses its rules rather than its balance.
    pub fn init(env: Env, admin: Address, policy: Policy) -> Result<(), Error> {
        if env.storage().instance().has(&Key::Admin) {
            return Err(Error::AlreadyInitialised);
        }
        admin.require_auth();
        policy::check(&policy)?;

        env.storage().instance().set(&Key::Admin, &admin);
        env.storage().instance().set(&Key::Policy, &policy);
        env.storage().instance().set(&Key::NextId, &1u32);
        env.storage()
            .instance()
            .set(&Key::Members, &Vec::<Address>::new(&env));
        env.storage()
            .instance()
            .set(&Key::Allowlist, &Vec::<Address>::new(&env));
        Ok(())
    }

    /// Reports the policy in force.
    pub fn policy(env: Env) -> Result<Policy, Error> {
        policy::load(&env)
    }

    /// Replaces the policy.
    pub fn set_policy(env: Env, policy: Policy) -> Result<(), Error> {
        policy::require_admin(&env)?;
        policy::check(&policy)?;
        env.storage().instance().set(&Key::Policy, &policy);
        Ok(())
    }

    /// Gives a member voting weight, or removes them by setting it to zero.
    pub fn set_member(env: Env, member: Address, weight: u32) -> Result<(), Error> {
        policy::require_admin(&env)?;
        policy::set_member(&env, &member, weight)
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

    /// Adds a recipient the treasury may pay.
    pub fn allow_recipient(env: Env, to: Address) -> Result<(), Error> {
        policy::require_admin(&env)?;
        policy::allow(&env, &to);
        Ok(())
    }

    /// Removes a recipient.
    pub fn deny_recipient(env: Env, to: Address) -> Result<(), Error> {
        policy::require_admin(&env)?;
        policy::deny(&env, &to);
        Ok(())
    }

    /// Reports the recipients this treasury may pay.
    pub fn allowlist(env: Env) -> Vec<Address> {
        policy::allowlist(&env)
    }

    /// Puts a payment to the members.
    pub fn propose(
        env: Env,
        proposer: Address,
        token: Address,
        to: Address,
        amount: i128,
        memo: BytesN<32>,
    ) -> Result<u32, Error> {
        proposals::propose(&env, &proposer, &token, &to, amount, &memo)
    }

    /// Records one member's vote.
    pub fn vote(env: Env, voter: Address, id: u32, approve: bool) -> Result<(), Error> {
        proposals::vote(&env, &voter, id, approve)
    }

    /// Reports a proposal.
    pub fn proposal(env: Env, id: u32) -> Result<Proposal, Error> {
        proposals::load(&env, id)
    }

    /// Closes voting and records the outcome.
    ///
    /// Separate from `execute` because a proposal that fails still has to stop
    /// being open, and nobody has an incentive to execute one that lost. Anyone
    /// may call it, for the same reason anyone may execute.
    pub fn tally(env: Env, id: u32) -> Result<Status, Error> {
        proposals::tally(&env, id)
    }

    /// Carries out a passed proposal.
    ///
    /// Deliberately takes no authorisation. See the module comment: the
    /// decision was made when the votes were cast, and requiring a particular
    /// person to be online afterwards would make this deployment a liveness
    /// dependency for somebody else's money.
    pub fn execute(env: Env, id: u32) -> Result<(), Error> {
        proposals::execute(&env, id)
    }

    /// Withdraws a proposal. Its proposer or the admin, and only while open.
    pub fn cancel(env: Env, id: u32) -> Result<(), Error> {
        proposals::cancel(&env, id)
    }

    /// Pays out under the rolling limit, without a proposal.
    ///
    /// Off by default: `spend_limit` starts at zero, which means every payment
    /// is a proposal. Turning it up is how a treasury trades review for speed
    /// on small amounts, and it is the treasury's trade to make.
    pub fn spend(
        env: Env,
        member: Address,
        token: Address,
        to: Address,
        amount: i128,
    ) -> Result<(), Error> {
        spend::spend(&env, &member, &token, &to, amount)
    }

    /// Reports how much of the rolling window is left.
    pub fn spendable(env: Env) -> Result<i128, Error> {
        spend::remaining(&env)
    }
}
