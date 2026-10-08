use soroban_sdk::{Address, Env, Vec};

use crate::types::{Error, Member, Policy};
use crate::Key;

/// How long this contract's state is kept alive, in ledgers, each time it is
/// used: about thirty days at five seconds a ledger, renewed once less than
/// half of that is left.
///
/// A treasury that nobody touches for a month is not one that should quietly
/// become unreadable, and anyone can renew it with `bump`. But the ordinary
/// case is that using it is what keeps it alive, so that a DAO which votes
/// every week never has to think about any of this.
pub(crate) const KEEP_LEDGERS: u32 = 518_400;
pub(crate) const RENEW_BELOW: u32 = KEEP_LEDGERS / 2;

/// Keeps the contract's own state alive. Called on every use.
pub(crate) fn keep_alive(env: &Env) {
    env.storage()
        .instance()
        .extend_ttl(RENEW_BELOW, KEEP_LEDGERS);
}

/// Keeps one stored entry alive.
pub(crate) fn keep<K: soroban_sdk::IntoVal<Env, soroban_sdk::Val>>(env: &Env, key: &K) {
    if env.storage().persistent().has(key) {
        env.storage()
            .persistent()
            .extend_ttl(key, RENEW_BELOW, KEEP_LEDGERS);
    }
}

/// Keeps alive everything that says who decides and who may be paid: the
/// contract's own state, each member's weight and each allowed recipient.
///
/// Proposals and votes are not included. They are renewed when they are
/// written, and an old one that lapses is a record that can be restored, not a
/// rule the treasury runs on.
pub(crate) fn keep_everything(env: &Env) {
    keep_alive(env);
    for member in members(env).iter() {
        keep(env, &Key::Member(member));
    }
    for to in allowlist(env).iter() {
        keep(env, &Key::Allowed(to));
    }
    keep(env, &Key::Window);
}

/// Writes a treasury's first state: its policy, its members and whom it may
/// pay. Once, when the contract is created, and by nothing afterwards.
///
/// Everything here is checked the way a later change to it would be. A treasury
/// that could not have been voted into this state should not be able to start
/// in it.
pub(crate) fn setup(
    env: &Env,
    policy: &Policy,
    founders: &Vec<Member>,
    payable: &Vec<Address>,
) -> Result<(), Error> {
    check(policy)?;

    let mut list = Vec::<Address>::new(env);
    let mut total: u32 = 0;
    for founder in founders.iter() {
        // A member with no weight is not a member, and listing one twice is a
        // mistake about who has how much say that nothing later would catch.
        if founder.weight == 0 || list.first_index_of(&founder.address).is_some() {
            return Err(Error::InvalidMembers);
        }
        env.storage()
            .persistent()
            .set(&Key::Member(founder.address.clone()), &founder.weight);
        list.push_back(founder.address.clone());
        total = total.saturating_add(founder.weight);
    }
    if total == 0 {
        return Err(Error::InvalidMembers);
    }
    if needed(total, policy.quorum_bps) > total {
        return Err(Error::WouldLockOut);
    }

    env.storage().instance().set(&Key::Policy, policy);
    env.storage().instance().set(&Key::Members, &list);
    env.storage().instance().set(&Key::NextId, &1u32);
    env.storage()
        .instance()
        .set(&Key::Allowlist, &Vec::<Address>::new(env));
    for to in payable.iter() {
        allow(env, &to);
    }
    Ok(())
}

/// Replaces the policy, having checked the one that would replace it.
pub(crate) fn replace(env: &Env, policy: &Policy) -> Result<(), Error> {
    check(policy)?;
    // A new quorum has to be one the current membership can still reach.
    let total = total_weight(env);
    if needed(total, policy.quorum_bps) > total {
        return Err(Error::WouldLockOut);
    }
    env.storage().instance().set(&Key::Policy, policy);
    Ok(())
}

/// Whether giving a member this weight would leave the treasury able to act.
/// The check `set_member` makes, without the write, for a proposal to be
/// refused when it is made rather than after a week of voting on it.
pub(crate) fn can_set_member(env: &Env, member: &Address, weight: u32) -> Result<(), Error> {
    let policy = load(env)?;
    let resulting = total_weight(env)
        .saturating_sub(weight_of(env, member))
        .saturating_add(weight);
    if resulting == 0 || needed(resulting, policy.quorum_bps) > resulting {
        return Err(Error::WouldLockOut);
    }
    Ok(())
}

/// The largest basis-point value, i.e. one hundred per cent.
const FULL_BPS: u32 = 10_000;

/// The longest any of a policy's periods may be: a year.
///
/// Not an opinion about how a DAO should run. Every one of these is added to a
/// timestamp, and a period long enough to overflow that sum would make every
/// later proposal fail — including the one that would have put it right.
pub(crate) const LONGEST_PERIOD: u64 = 365 * 86_400;

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
    if policy.voting_period > LONGEST_PERIOD
        || policy.timelock > LONGEST_PERIOD
        || policy.spend_window > LONGEST_PERIOD
    {
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
    // Taking part is what keeps a member's place alive.
    keep(env, &Key::Member(member.clone()));
    Ok(weight)
}
