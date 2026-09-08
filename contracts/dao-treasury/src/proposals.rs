use soroban_sdk::{token, Address, BytesN, Env};

use crate::policy;
use crate::types::{Error, Proposal, Status};
use crate::Key;

pub(crate) fn load(env: &Env, id: u32) -> Result<Proposal, Error> {
    env.storage()
        .persistent()
        .get(&Key::Proposal(id))
        .ok_or(Error::UnknownProposal)
}

fn save(env: &Env, proposal: &Proposal) {
    env.storage()
        .persistent()
        .set(&Key::Proposal(proposal.id), proposal);
}

/// Opens a proposal.
///
/// The recipient is checked here as well as at execution. Checking only at
/// execution would let a treasury spend a week voting on a payment that was
/// never going to be possible, which wastes the one resource a DAO actually has.
pub(crate) fn propose(
    env: &Env,
    proposer: &Address,
    token: &Address,
    to: &Address,
    amount: i128,
    memo: &BytesN<32>,
) -> Result<u32, Error> {
    let policy = policy::load(env)?;
    policy::require_member(env, proposer)?;

    if amount <= 0 {
        return Err(Error::InvalidAmount);
    }
    if !policy::is_allowed(env, to) {
        return Err(Error::RecipientNotAllowed);
    }

    let id: u32 = env.storage().instance().get(&Key::NextId).unwrap_or(1);
    env.storage().instance().set(&Key::NextId, &(id + 1));

    let now = env.ledger().timestamp();
    let proposal = Proposal {
        id,
        proposer: proposer.clone(),
        token: token.clone(),
        to: to.clone(),
        amount,
        memo: memo.clone(),
        status: Status::Open,
        closes_at: now + policy.voting_period,
        // Measured from when voting closes, not from when it passed. Otherwise
        // a proposal that reaches its threshold early shortens its own
        // timelock, and the timelock is precisely the window in which the rest
        // of the DAO notices something is wrong.
        executable_at: now + policy.voting_period + policy.timelock,
        yes: 0,
        no: 0,
    };
    save(env, &proposal);
    Ok(id)
}

/// Records one member's vote, once.
///
/// The weight is read now rather than at proposal time, so a member whose
/// weight changed mid-vote votes with what they currently have. The alternative
/// — snapshotting at proposal time — sounds fairer and means a removed member
/// keeps voting, which is worse.
pub(crate) fn vote(env: &Env, voter: &Address, id: u32, approve: bool) -> Result<(), Error> {
    let weight = policy::require_member(env, voter)?;
    let mut proposal = load(env, id)?;

    if proposal.status != Status::Open || env.ledger().timestamp() >= proposal.closes_at {
        return Err(Error::NotOpen);
    }

    let key = Key::Voted(id, voter.clone());
    if env.storage().persistent().has(&key) {
        return Err(Error::AlreadyVoted);
    }
    env.storage().persistent().set(&key, &approve);

    if approve {
        proposal.yes = proposal.yes.saturating_add(weight);
    } else {
        proposal.no = proposal.no.saturating_add(weight);
    }
    save(env, &proposal);
    Ok(())
}

/// Closes voting and records the outcome.
///
/// Anyone may call it. A proposal that lost has nobody with an incentive to
/// close it, and one that stayed Open forever would block nothing but would
/// misreport itself to everyone reading.
pub(crate) fn tally(env: &Env, id: u32) -> Result<Status, Error> {
    let mut proposal = load(env, id)?;
    if proposal.status != Status::Open {
        return Ok(proposal.status);
    }
    if env.ledger().timestamp() < proposal.closes_at {
        return Err(Error::NotOpen);
    }

    proposal.status = outcome(env, &proposal)?;
    save(env, &proposal);
    Ok(proposal.status)
}

/// Decides whether a closed proposal passed.
///
/// Quorum against total weight, then approval against the weight that actually
/// voted. Two separate questions — how many turned up, and how many of those
/// agreed — and collapsing them into one number is how a treasury with three
/// active members out of thirty discovers it has been passing everything.
fn outcome(env: &Env, proposal: &Proposal) -> Result<Status, Error> {
    let policy = policy::load(env)?;
    let total = policy::total_weight(env);
    let cast = proposal.yes.saturating_add(proposal.no);

    if cast < policy::needed(total, policy.quorum_bps) {
        return Ok(Status::Rejected);
    }
    if proposal.yes < policy::needed(cast, policy.approval_bps) {
        return Ok(Status::Rejected);
    }
    Ok(Status::Passed)
}

/// Carries out a passed proposal.
///
/// No `require_auth` anywhere in this path, deliberately. The decision was made
/// when the votes were cast and the timelock has run; requiring a particular
/// signer afterwards would make somebody's availability a condition of the
/// treasury's.
///
/// What stops a stranger draining the treasury is not authorisation, it is that
/// there is nothing here for a stranger to choose: the recipient, the token and
/// the amount were fixed when the proposal was written, and the votes are what
/// let this run at all.
pub(crate) fn execute(env: &Env, id: u32) -> Result<(), Error> {
    let mut proposal = load(env, id)?;

    // Voting may have closed without anyone bothering to tally. Doing it here
    // means a passed proposal cannot be stranded by nobody having called the
    // other function.
    if proposal.status == Status::Open {
        if env.ledger().timestamp() < proposal.closes_at {
            return Err(Error::NotExecutable);
        }
        proposal.status = outcome(env, &proposal)?;
        save(env, &proposal);
    }

    if proposal.status != Status::Passed {
        return Err(Error::NotExecutable);
    }
    if env.ledger().timestamp() < proposal.executable_at {
        return Err(Error::NotExecutable);
    }
    // Re-checked at execution, not only at proposal time. An address removed
    // from the allowlist during the timelock is an address somebody decided
    // should not be paid, and the timelock exists precisely so that decision
    // can land in time.
    if !policy::is_allowed(env, &proposal.to) {
        return Err(Error::RecipientNotAllowed);
    }

    // Marked executed before the transfer. If the transfer traps the whole
    // invocation reverts and the mark goes with it; if it succeeds the mark is
    // already there. The other order leaves a window where a re-entrant token
    // could execute the same proposal twice.
    proposal.status = Status::Executed;
    save(env, &proposal);

    token::Client::new(env, &proposal.token).transfer(
        &env.current_contract_address(),
        &proposal.to,
        &proposal.amount,
    );
    Ok(())
}

/// Withdraws a proposal.
pub(crate) fn cancel(env: &Env, id: u32) -> Result<(), Error> {
    let mut proposal = load(env, id)?;
    if proposal.status != Status::Open {
        return Err(Error::NotOpen);
    }

    // Its proposer, or the admin. Not any member: letting one member cancel
    // another's proposal turns a vote into a race.
    let admin = policy::admin(env)?;
    if env
        .storage()
        .persistent()
        .has(&Key::Member(proposal.proposer.clone()))
    {
        proposal.proposer.require_auth();
    } else {
        admin.require_auth();
    }

    proposal.status = Status::Cancelled;
    save(env, &proposal);
    Ok(())
}
