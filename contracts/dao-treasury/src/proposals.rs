use soroban_sdk::{Address, BytesN, Env};

use crate::events::{Decided, Executed, Proposed, Voted};
use crate::policy;
use crate::types::{Action, Error, Proposal, Status};
use crate::Key;

pub(crate) fn load(env: &Env, id: u32) -> Result<Proposal, Error> {
    env.storage()
        .persistent()
        .get(&Key::Proposal(id))
        .ok_or(Error::UnknownProposal)
}

fn save(env: &Env, proposal: &Proposal) {
    let key = Key::Proposal(proposal.id);
    env.storage().persistent().set(&key, proposal);
    policy::keep(env, &key);
}

/// Checks that an action is one that could be carried out as things stand.
///
/// At proposal time as well as at execution. Checking only at execution would
/// let a treasury spend a week voting on something that was never going to be
/// possible, which wastes the one resource a DAO actually has.
fn check(env: &Env, action: &Action) -> Result<(), Error> {
    match action {
        Action::Pay(payment) => {
            if payment.amount <= 0 {
                return Err(Error::InvalidAmount);
            }
            if !policy::is_allowed(env, &payment.to) {
                return Err(Error::RecipientNotAllowed);
            }
            Ok(())
        }
        Action::SetPolicy(new) => policy::check(new),
        Action::SetMember(member) => policy::can_set_member(env, &member.address, member.weight),
        Action::AllowRecipient(_) | Action::DenyRecipient(_) => Ok(()),
        Action::Revoke(target) => revocable(env, *target).map(|_| ()),
    }
}

/// Loads a proposal that can still be stopped: one that is open, or has passed
/// and not yet been carried out.
fn revocable(env: &Env, id: u32) -> Result<Proposal, Error> {
    let target = load(env, id)?;
    match target.status {
        Status::Open | Status::Passed => Ok(target),
        _ => Err(Error::NothingToRevoke),
    }
}

/// Opens a proposal.
pub(crate) fn propose(
    env: &Env,
    proposer: &Address,
    action: &Action,
    memo: &BytesN<32>,
) -> Result<u32, Error> {
    let policy = policy::load(env)?;
    policy::require_member(env, proposer)?;
    check(env, action)?;

    let id: u32 = env.storage().instance().get(&Key::NextId).unwrap_or(1);
    env.storage().instance().set(&Key::NextId, &(id + 1));

    let now = env.ledger().timestamp();
    let closes_at = now + policy.voting_period;
    let proposal = Proposal {
        id,
        proposer: proposer.clone(),
        action: action.clone(),
        memo: memo.clone(),
        status: Status::Open,
        closes_at,
        // The latest it can become executable: when voting closes, plus the
        // timelock. Settling the outcome early brings this forward, but never
        // to less than a whole timelock after the moment it was settled — the
        // timelock is the window in which the rest of the DAO notices
        // something is wrong, and no way of passing shortens it.
        executable_at: closes_at + wait(&policy, action),
        yes: 0,
        no: 0,
    };
    save(env, &proposal);

    Proposed {
        id,
        proposer: proposer.clone(),
        memo: memo.clone(),
        closes_at,
    }
    .publish(env);
    Ok(id)
}

/// How long an action waits between being decided and being carried out.
fn wait(policy: &crate::types::Policy, action: &Action) -> u64 {
    if action.restricts() {
        0
    } else {
        policy.timelock
    }
}

/// Records one member's vote, once, and settles the proposal if that vote
/// settles it.
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
    policy::keep(env, &key);

    if approve {
        proposal.yes = proposal.yes.saturating_add(weight);
    } else {
        proposal.no = proposal.no.saturating_add(weight);
    }

    Voted {
        id,
        voter: voter.clone(),
        approve,
        weight,
    }
    .publish(env);

    settle(env, &mut proposal)?;
    save(env, &proposal);
    Ok(())
}

/// Reports how a member voted on a proposal, or nothing if they have not.
pub(crate) fn voted(env: &Env, id: u32, voter: &Address) -> Option<bool> {
    env.storage()
        .persistent()
        .get(&Key::Voted(id, voter.clone()))
}

/// Settles a proposal if its outcome is no longer in question, and records
/// when it may be carried out.
///
/// Two ways to be settled. Voting has closed, and the votes are what they are.
/// Or voting is still open and nothing that could yet be cast would change the
/// result: it has enough in favour to pass if everybody else voted against, or
/// enough against that it could not pass if everybody else voted for.
///
/// The second is not a shortcut around anything. It ends the wait for votes
/// that cannot matter, and the timelock then runs in full from that moment. A
/// treasury of three that all agree on Monday should not have to wait until the
/// voting window closes on Friday to start a timelock they have already
/// agreed to sit through.
fn settle(env: &Env, proposal: &mut Proposal) -> Result<(), Error> {
    if proposal.status != Status::Open {
        return Ok(());
    }
    let policy = policy::load(env)?;
    let total = policy::total_weight(env);
    let now = env.ledger().timestamp();

    let status = if now >= proposal.closes_at {
        closed_outcome(&policy, total, proposal)
    } else {
        match early_outcome(&policy, total, proposal) {
            Some(status) => status,
            None => return Ok(()),
        }
    };

    proposal.status = status;
    if status == Status::Passed {
        // A full wait from now, or the end of voting plus the wait, whichever
        // is sooner. Never less than a full wait after being settled.
        let from_now = now + wait(&policy, &proposal.action);
        if from_now < proposal.executable_at {
            proposal.executable_at = from_now;
        }
    }

    Decided {
        id: proposal.id,
        status,
        yes: proposal.yes,
        no: proposal.no,
        executable_at: proposal.executable_at,
    }
    .publish(env);
    Ok(())
}

/// Decides a proposal whose voting has closed.
///
/// Quorum against total weight, then approval against the weight that actually
/// voted. Two separate questions — how many turned up, and how many of those
/// agreed — and collapsing them into one number is how a treasury with three
/// active members out of thirty discovers it has been passing everything.
fn closed_outcome(policy: &crate::types::Policy, total: u32, proposal: &Proposal) -> Status {
    let cast = proposal.yes.saturating_add(proposal.no);
    if cast < policy::needed(total, policy.quorum_bps) {
        return Status::Rejected;
    }
    if proposal.yes < policy::needed(cast, policy.approval_bps) {
        return Status::Rejected;
    }
    Status::Passed
}

/// Decides a proposal whose voting is still open, if it can be decided.
///
/// Both tests assume the worst for the side being tested. For one that is
/// passing: that every vote not yet cast goes against it, or is never cast at
/// all. For one that is failing: that every vote not yet cast goes for it.
fn early_outcome(policy: &crate::types::Policy, total: u32, proposal: &Proposal) -> Option<Status> {
    if total == 0 {
        return None;
    }
    let pass_mark = policy::needed(total, policy.approval_bps);
    // Enough in favour that no later vote could outweigh it — and enough cast
    // already. Where a quorum asks for more than approval does, staying away
    // is still a way to stop a proposal, and settling it early on approval
    // alone would take that away.
    let cast = proposal.yes.saturating_add(proposal.no);
    if proposal.yes >= pass_mark && cast >= policy::needed(total, policy.quorum_bps) {
        return Some(Status::Passed);
    }
    // The most it could still get in favour is everything not cast against.
    if total.saturating_sub(proposal.no) < pass_mark {
        return Some(Status::Rejected);
    }
    None
}

/// Settles a proposal whose outcome is no longer in question.
///
/// Anyone may call it. A proposal that lost has nobody with an incentive to
/// close it, and one that stayed Open forever would block nothing but would
/// misreport itself to everyone reading.
pub(crate) fn tally(env: &Env, id: u32) -> Result<Status, Error> {
    let mut proposal = load(env, id)?;
    if proposal.status != Status::Open {
        return Ok(proposal.status);
    }
    settle(env, &mut proposal)?;
    if proposal.status == Status::Open {
        return Err(Error::Undecided);
    }
    save(env, &proposal);
    Ok(proposal.status)
}

/// Carries out a passed proposal.
///
/// No `require_auth` anywhere in this path, deliberately. The decision was made
/// when the votes were cast and the timelock has run; requiring a particular
/// signer afterwards would make somebody's availability a condition of the
/// treasury's.
///
/// What stops a stranger doing harm here is not authorisation, it is that there
/// is nothing here for a stranger to choose: what the proposal does was fixed
/// when it was written, and the votes are what let this run at all.
pub(crate) fn execute(env: &Env, id: u32) -> Result<(), Error> {
    let mut proposal = load(env, id)?;

    // Voting may have closed without anyone bothering to tally. Doing it here
    // means a passed proposal cannot be stranded by nobody having called the
    // other function.
    //
    // Nothing is saved on the way to a refusal, because a refusal reverts
    // everything this invocation wrote. A proposal that has been decided and is
    // not yet executable is recorded as decided by `tally`, not by a failed
    // attempt to execute it.
    settle(env, &mut proposal)?;
    if proposal.status != Status::Passed {
        return Err(Error::NotExecutable);
    }
    if env.ledger().timestamp() < proposal.executable_at {
        return Err(Error::NotExecutable);
    }

    // Marked executed before anything is done. If what follows traps, the whole
    // invocation reverts and the mark goes with it; if it succeeds the mark is
    // already there. The other order leaves a window where a re-entrant token
    // could execute the same proposal twice.
    proposal.status = Status::Executed;
    save(env, &proposal);

    match &proposal.action {
        Action::Pay(payment) => {
            // Re-checked at execution, not only at proposal time. An address
            // removed from the allowlist during the timelock is an address
            // somebody decided should not be paid, and the timelock exists
            // precisely so that decision can land in time.
            if !policy::is_allowed(env, &payment.to) {
                return Err(Error::RecipientNotAllowed);
            }
            crate::spend::pay_out(env, &payment.token, &payment.to, payment.amount)?;
        }
        // Each of these is checked again here, against the treasury as it is
        // now. Membership and policy can both have changed since the vote, and
        // a change that was safe then may strand the treasury today.
        Action::SetPolicy(new) => policy::replace(env, new)?,
        Action::SetMember(member) => policy::set_member(env, &member.address, member.weight)?,
        Action::AllowRecipient(to) => policy::allow(env, to),
        Action::DenyRecipient(to) => policy::deny(env, to),
        Action::Revoke(target) => {
            // Whatever it was going to do, it now never does. If it has been
            // carried out in the meantime there is nothing left to stop, and
            // this says so rather than reporting a success that changed
            // nothing.
            let mut stopped = revocable(env, *target)?;
            stopped.status = Status::Cancelled;
            save(env, &stopped);
            Decided {
                id: stopped.id,
                status: Status::Cancelled,
                yes: stopped.yes,
                no: stopped.no,
                executable_at: stopped.executable_at,
            }
            .publish(env);
        }
    }

    Executed { id }.publish(env);
    Ok(())
}

/// Withdraws a proposal: whoever made it, and only while it is open.
///
/// Nobody else. Letting one member cancel another's proposal turns a vote into
/// a race, and there is no longer anybody whose key stands above the members'.
/// A proposal its proposer cannot or will not withdraw is voted down like any
/// other, or left to close — and one that has already passed is no longer its
/// proposer's to withdraw. The members stop that one, with `Action::Revoke`.
pub(crate) fn cancel(env: &Env, proposer: &Address, id: u32) -> Result<(), Error> {
    let mut proposal = load(env, id)?;
    if proposal.status != Status::Open {
        return Err(Error::NotOpen);
    }
    if proposal.proposer != *proposer {
        return Err(Error::NotProposer);
    }
    proposer.require_auth();

    proposal.status = Status::Cancelled;
    save(env, &proposal);
    Decided {
        id,
        status: Status::Cancelled,
        yes: proposal.yes,
        no: proposal.no,
        executable_at: proposal.executable_at,
    }
    .publish(env);
    Ok(())
}
