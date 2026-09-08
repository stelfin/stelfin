use soroban_sdk::{Address, Env, Vec};

use crate::types::{Error, Policy};
use crate::Key;

/// The largest basis-point value, i.e. one hundred per cent.
const FULL_BPS: u32 = 10_000;

/// Rejects a policy that could never pass anything, or would pass everything.
///
/// Checked at the boundary rather than trusted, because both extremes are quiet
/// failures. A quorum above 100% is a treasury that can never spend again and
/// whose members will discover it at the worst moment; an approval threshold of
/// zero is a treasury where a single "no" vote passes a proposal.
pub(crate) fn check(policy: &Policy) -> Result<(), Error> {
    if policy.quorum_bps == 0 || policy.quorum_bps > FULL_BPS {
        return Err(Error::InvalidPolicy);
    }
    // Strictly above half. At exactly half, a tie passes — and a treasury where
    // an even split spends the money is not one anybody described.
    if policy.approval_bps <= FULL_BPS / 2 || policy.approval_bps > FULL_BPS {
        return Err(Error::InvalidPolicy);
    }
    if policy.voting_period == 0 {
        return Err(Error::InvalidPolicy);
    }
    if policy.spend_limit < 0 {
        return Err(Error::InvalidPolicy);
    }
    // A limit with no window is a limit that never resets: the first payment
    // consumes it forever, which reads as a bug rather than a policy.
    if policy.spend_limit > 0 && policy.spend_window == 0 {
        return Err(Error::InvalidPolicy);
    }
    Ok(())
}

pub(crate) fn load(env: &Env) -> Result<Policy, Error> {
    env.storage()
        .instance()
        .get(&Key::Policy)
        .ok_or(Error::NotInitialised)
}

pub(crate) fn admin(env: &Env) -> Result<Address, Error> {
    env.storage()
        .instance()
        .get(&Key::Admin)
        .ok_or(Error::NotInitialised)
}

/// Requires the admin's signature.
pub(crate) fn require_admin(env: &Env) -> Result<(), Error> {
    admin(env)?.require_auth();
    Ok(())
}

pub(crate) fn weight_of(env: &Env, member: &Address) -> u32 {
    env.storage()
        .persistent()
        .get(&Key::Member(member.clone()))
        .unwrap_or(0)
}

pub(crate) fn members(env: &Env) -> Vec<Address> {
    env.storage()
        .instance()
        .get(&Key::Members)
        .unwrap_or_else(|| Vec::new(env))
}

pub(crate) fn total_weight(env: &Env) -> u32 {
    let mut total: u32 = 0;
    for member in members(env).iter() {
        total = total.saturating_add(weight_of(env, &member));
    }
    total
}

/// Sets a member's weight, refusing a change that would strand the treasury.
///
/// The check is the same shape as the classic-multisig lockout check next door,
/// and for the same reason: this is the one operation that can make a treasury
/// permanently unable to spend, with no recourse and no signal until somebody
/// tries. Removing the last member, or dropping total weight below what quorum
/// needs, is refused at the write.
pub(crate) fn set_member(env: &Env, member: &Address, weight: u32) -> Result<(), Error> {
    let policy = load(env)?;
    let mut list = members(env);
    let current = weight_of(env, member);

    let resulting = total_weight(env)
        .saturating_sub(current)
        .saturating_add(weight);

    if resulting == 0 {
        return Err(Error::WouldLockOut);
    }
    // Quorum is a share of total weight, so it scales with membership and
    // cannot be stranded by removal alone. What can be stranded is the case
    // where the arithmetic rounds a quorum up past everything that remains.
    if needed(resulting, policy.quorum_bps) > resulting {
        return Err(Error::WouldLockOut);
    }

    if weight == 0 {
        env.storage()
            .persistent()
            .remove(&Key::Member(member.clone()));
        if let Some(at) = list.first_index_of(member) {
            list.remove(at);
        }
    } else {
        env.storage()
            .persistent()
            .set(&Key::Member(member.clone()), &weight);
        if list.first_index_of(member).is_none() {
            list.push_back(member.clone());
        }
    }
    env.storage().instance().set(&Key::Members, &list);
    Ok(())
}

/// needed is `total * bps / 10000`, rounded up.
///
/// Rounded up so a threshold of "more than half" of three weight is two rather
/// than one. Rounding down would make every odd-sized treasury quietly easier
/// to pass a proposal in than its own policy says.
pub(crate) fn needed(total: u32, bps: u32) -> u32 {
    // u64 throughout: total * 10000 overflows u32 at a total of 429,497, which
    // is a plausible weight for a large DAO and a wrong answer rather than a
    // panic.
    let product = (total as u64) * (bps as u64);
    let rounded = product.div_ceil(FULL_BPS as u64);
    // The ceiling cannot exceed total for bps <= 10000, but clamping is
    // cheaper than reasoning about it every time this is read.
    rounded.min(total as u64) as u32
}

pub(crate) fn allow(env: &Env, to: &Address) {
    env.storage()
        .persistent()
        .set(&Key::Allowed(to.clone()), &true);
    let mut list = allowlist(env);
    if list.first_index_of(to).is_none() {
        list.push_back(to.clone());
        env.storage().instance().set(&Key::Allowlist, &list);
    }
}

pub(crate) fn deny(env: &Env, to: &Address) {
    env.storage().persistent().remove(&Key::Allowed(to.clone()));
    let mut list = allowlist(env);
    if let Some(at) = list.first_index_of(to) {
        list.remove(at);
        env.storage().instance().set(&Key::Allowlist, &list);
    }
}

pub(crate) fn allowlist(env: &Env) -> Vec<Address> {
    env.storage()
        .instance()
        .get(&Key::Allowlist)
        .unwrap_or_else(|| Vec::new(env))
}

/// Whether the treasury may pay this recipient at all.
///
/// An empty allowlist means nobody, not everybody. The opposite default is the
/// one that reads as convenient and ships a treasury that can pay an attacker's
/// address on the day somebody forgets to populate it.
pub(crate) fn is_allowed(env: &Env, to: &Address) -> bool {
    env.storage()
        .persistent()
        .get(&Key::Allowed(to.clone()))
        .unwrap_or(false)
}

/// Requires a member's signature and reports their weight.
pub(crate) fn require_member(env: &Env, member: &Address) -> Result<u32, Error> {
    let weight = weight_of(env, member);
    if weight == 0 {
        return Err(Error::NotMember);
    }
    member.require_auth();
    Ok(weight)
}
