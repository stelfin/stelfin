use soroban_sdk::{Address, BytesN, Env, Symbol, Vec};

use crate::types::{Error, Grant, Revocation, Standing};
use crate::Key;

/// The width of the rolling window a daily limit is measured over.
const DAY: u64 = 86_400;

/// What one connector has moved inside the window currently open.
#[soroban_sdk::contracttype]
#[derive(Clone, Debug)]
pub(crate) struct Used {
    pub start: u64,
    pub amount: i128,
}

pub(crate) fn admin(env: &Env) -> Result<Address, Error> {
    env.storage()
        .instance()
        .get(&Key::Admin)
        .ok_or(Error::NotInitialised)
}

pub(crate) fn require_admin(env: &Env) -> Result<(), Error> {
    admin(env)?.require_auth();
    Ok(())
}

pub(crate) fn connectors(env: &Env) -> Vec<Symbol> {
    env.storage()
        .instance()
        .get(&Key::Connectors)
        .unwrap_or_else(|| Vec::new(env))
}

pub(crate) fn load(env: &Env, connector: &Symbol) -> Result<Grant, Error> {
    env.storage()
        .persistent()
        .get(&Key::Grant(connector.clone()))
        .ok_or(Error::UnknownGrant)
}

/// Records a grant, replacing whatever was there.
///
/// A re-grant clears any revocation: the DAO has decided again, and leaving the
/// old revocation would make the new grant read as withdrawn. The daily window
/// is cleared too, because the limits may have changed and carrying usage
/// against a different limit is arithmetic about two different things.
pub(crate) fn grant(env: &Env, connector: &Symbol, grant: &Grant) -> Result<(), Error> {
    check(grant)?;

    env.storage()
        .persistent()
        .set(&Key::Grant(connector.clone()), grant);
    env.storage()
        .persistent()
        .remove(&Key::Revoked(connector.clone()));
    env.storage()
        .persistent()
        .remove(&Key::Used(connector.clone()));

    let mut list = connectors(env);
    if list.first_index_of(connector).is_none() {
        list.push_back(connector.clone());
        env.storage().instance().set(&Key::Connectors, &list);
    }
    Ok(())
}

/// Refuses a grant that says something it does not mean.
fn check(grant: &Grant) -> Result<(), Error> {
    if grant.not_after <= grant.not_before {
        return Err(Error::InvalidGrant);
    }
    if grant.per_call_limit < 0 || grant.daily_limit < 0 {
        return Err(Error::InvalidGrant);
    }
    // A per-call limit above the daily one is a number that can never apply,
    // and reads to a person as the real limit. Refused rather than silently
    // clamped, because the DAO wrote it and should be told it does not mean
    // what it looks like.
    if grant.per_call_limit > grant.daily_limit {
        return Err(Error::InvalidGrant);
    }
    Ok(())
}

/// Withdraws a grant and records why.
pub(crate) fn revoke(
    env: &Env,
    by: &Address,
    connector: &Symbol,
    reason: &Symbol,
) -> Result<(), Error> {
    load(env, connector)?;
    if env
        .storage()
        .persistent()
        .has(&Key::Revoked(connector.clone()))
    {
        return Err(Error::AlreadyRevoked);
    }

    // The grant itself is kept. Deleting it would make "revoked" and "never
    // granted" the same observation, and an auditor asking why a connector
    // stopped working deserves an answer.
    env.storage().persistent().set(
        &Key::Revoked(connector.clone()),
        &Revocation {
            at: env.ledger().timestamp(),
            by: by.clone(),
            reason: reason.clone(),
        },
    );
    Ok(())
}

pub(crate) fn standing(env: &Env, connector: &Symbol) -> Standing {
    let Ok(grant) = load(env, connector) else {
        return Standing::None;
    };
    if env
        .storage()
        .persistent()
        .has(&Key::Revoked(connector.clone()))
    {
        return Standing::Revoked;
    }

    let now = env.ledger().timestamp();
    if now < grant.not_before {
        Standing::NotYet
    } else if now >= grant.not_after {
        Standing::Expired
    } else {
        Standing::Active
    }
}

fn used(env: &Env, connector: &Symbol) -> Used {
    let now = env.ledger().timestamp();
    match env
        .storage()
        .persistent()
        .get::<_, Used>(&Key::Used(connector.clone()))
    {
        Some(u) if now < u.start + DAY => u,
        // A fresh window starts now rather than at a clock boundary: aligning
        // to one would let a caller wait for the tick and spend two days'
        // allowance back to back.
        _ => Used {
            start: now,
            amount: 0,
        },
    }
}

/// Checks one call against a grant and records what it consumed.
///
/// The order is deliberate. Standing first, because an expired or revoked grant
/// makes every other question irrelevant. Then the pins — surface and endpoint
/// — because they decide whether this is even the connector that was granted
/// anything. Limits last, so a call that was never going to be allowed does not
/// consume a day's allowance on its way to being refused.
pub(crate) fn authorise(
    env: &Env,
    connector: &Symbol,
    surface_digest: &BytesN<32>,
    endpoint_hash: &BytesN<32>,
    amount: i128,
) -> Result<(), Error> {
    let grant = load(env, connector)?;

    if standing(env, connector) != Standing::Active {
        return Err(Error::NotActive);
    }
    // The connector's own description of what it offers has changed since the
    // DAO agreed to it. A server granted "read a spreadsheet" that now answers
    // with a tool transferring funds passes every capability check and fails
    // this one.
    if &grant.surface_digest != surface_digest {
        return Err(Error::SurfaceChanged);
    }
    if &grant.endpoint_hash != endpoint_hash {
        return Err(Error::WrongEndpoint);
    }

    if amount < 0 {
        return Err(Error::OverLimit);
    }
    if amount > grant.per_call_limit {
        return Err(Error::OverLimit);
    }

    let mut window = used(env, connector);
    let total = window.amount.checked_add(amount).ok_or(Error::OverLimit)?;
    if total > grant.daily_limit {
        return Err(Error::OverLimit);
    }

    window.amount = total;
    env.storage()
        .persistent()
        .set(&Key::Used(connector.clone()), &window);
    Ok(())
}

pub(crate) fn remaining(env: &Env, connector: &Symbol) -> Result<i128, Error> {
    let grant = load(env, connector)?;
    if standing(env, connector) != Standing::Active {
        return Ok(0);
    }
    Ok((grant.daily_limit - used(env, connector).amount).max(0))
}
