use soroban_sdk::{token, Address, Env};

use crate::policy;
use crate::types::Error;
use crate::Key;

/// How much has been spent in the window that is currently open.
///
/// A rolling window kept as (start, spent) rather than a list of payments. A
/// list would be exact and would grow without bound in a contract that pays
/// per-item storage rent, which is a different way to break a treasury.
#[soroban_sdk::contracttype]
#[derive(Clone, Debug)]
pub(crate) struct Window {
    pub start: u64,
    pub spent: i128,
}

fn current(env: &Env, width: u64) -> Window {
    let now = env.ledger().timestamp();
    match env.storage().persistent().get::<_, Window>(&Key::Window) {
        Some(w) if width > 0 && now < w.start + width => w,
        // Expired, or never opened. A fresh window starts now rather than at a
        // multiple of the width: aligning to a boundary would let a caller wait
        // for the tick and spend two windows' worth back to back.
        _ => Window {
            start: now,
            spent: 0,
        },
    }
}

/// Pays out under the rolling limit, without a proposal.
pub(crate) fn spend(
    env: &Env,
    member: &Address,
    token_id: &Address,
    to: &Address,
    amount: i128,
) -> Result<(), Error> {
    let policy = policy::load(env)?;
    policy::require_member(env, member)?;

    if amount <= 0 {
        return Err(Error::InvalidAmount);
    }
    if !policy::is_allowed(env, to) {
        return Err(Error::RecipientNotAllowed);
    }
    // Zero is the default and means every payment is a proposal. Reported as a
    // limit that was exceeded, which is exactly what happened.
    if policy.spend_limit == 0 {
        return Err(Error::SpendLimitExceeded);
    }

    let mut window = current(env, policy.spend_window);
    let spent = window
        .spent
        .checked_add(amount)
        .ok_or(Error::SpendLimitExceeded)?;
    if spent > policy.spend_limit {
        return Err(Error::SpendLimitExceeded);
    }

    // Written before the transfer, same reasoning as execute: a re-entrant
    // token that called back in would otherwise see the old total and be
    // allowed to spend the window twice.
    window.spent = spent;
    env.storage().persistent().set(&Key::Window, &window);

    token::Client::new(env, token_id).transfer(&env.current_contract_address(), to, &amount);
    Ok(())
}

/// How much is left in the window that is currently open.
pub(crate) fn remaining(env: &Env) -> Result<i128, Error> {
    let policy = policy::load(env)?;
    let window = current(env, policy.spend_window);
    Ok((policy.spend_limit - window.spent).max(0))
}
