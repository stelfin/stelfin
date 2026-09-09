#![no_std]

//! Which connectors a DAO has allowed, and what each may do.
//!
//! stelfin will let a community reach a spreadsheet, a model, a trading venue —
//! anything — from its chat. The question that makes that safe is not "can the
//! bot reach it" but "did the DAO say it could, and can anyone check". This
//! contract is the answer to the second half: a grant here is public,
//! timestamped and revocable by the DAO itself, and stelfin cannot alter it.
//!
//! # A ledger is not a safe
//!
//! Everything written to a contract is readable by everyone. There is no
//! private storage, only storage that has not been looked at yet. So no
//! credential, no bearer token and no endpoint URL is ever stored here — the
//! endpoint is committed to as a salted hash, which proves the connector being
//! called is the registered one without publishing where it is.
//!
//! # Why the surface digest matters more than the capability list
//!
//! A capability list says what a connector may be asked for. The surface digest
//! says what the connector claimed it offers. Without the second, a server
//! granted "read a spreadsheet" can later answer the same call with a tool that
//! transfers funds, and every check upstream still passes because the
//! capability name did not change. Pinning the digest turns that into a refusal.

mod grants;
mod types;

#[cfg(test)]
mod test;

use soroban_sdk::{contract, contractimpl, contractmeta, Address, BytesN, Env, Symbol, Vec};

pub use types::{Error, Grant, Revocation, Standing};

contractmeta!(
    key = "Description",
    val = "Capability grants for the tools a DAO connects to its treasury."
);

#[soroban_sdk::contracttype]
#[derive(Clone)]
pub(crate) enum Key {
    Admin,
    /// A grant, by connector id.
    Grant(Symbol),
    /// Why a grant stopped applying, kept as a positive fact.
    Revoked(Symbol),
    /// Every connector id, so the set can be read back.
    Connectors,
    /// What one connector has moved inside the current rolling day.
    Used(Symbol),
}

#[contract]
pub struct ConnectorRegistry;

#[contractimpl]
impl ConnectorRegistry {
    /// Sets the registry up, once.
    ///
    /// The admin is the DAO — in practice its treasury contract or its
    /// multisig, not stelfin. That is the whole design: an operator who can
    /// grant capabilities to itself has not been constrained by anything.
    pub fn init(env: Env, admin: Address) -> Result<(), Error> {
        if env.storage().instance().has(&Key::Admin) {
            return Err(Error::AlreadyInitialised);
        }
        admin.require_auth();
        env.storage().instance().set(&Key::Admin, &admin);
        env.storage()
            .instance()
            .set(&Key::Connectors, &Vec::<Symbol>::new(&env));
        Ok(())
    }

    /// Reports who may change this registry.
    pub fn admin(env: Env) -> Result<Address, Error> {
        grants::admin(&env)
    }

    /// Grants a connector its capabilities, replacing any previous grant.
    ///
    /// Replacing rather than amending: a grant is the complete statement of
    /// what a connector may do, and an amendment API is how a capability
    /// nobody remembers adding survives three reviews.
    pub fn grant(env: Env, connector: Symbol, grant: Grant) -> Result<(), Error> {
        grants::require_admin(&env)?;
        grants::grant(&env, &connector, &grant)
    }

    /// Withdraws a grant, recording why.
    pub fn revoke(env: Env, connector: Symbol, reason: Symbol) -> Result<(), Error> {
        let admin = grants::admin(&env)?;
        admin.require_auth();
        grants::revoke(&env, &admin, &connector, &reason)
    }

    /// Reports a connector's grant, whether or not it is currently valid.
    pub fn grant_of(env: Env, connector: Symbol) -> Result<Grant, Error> {
        grants::load(&env, &connector)
    }

    /// Reports whether a grant applies right now, and why not if it does not.
    pub fn standing(env: Env, connector: Symbol) -> Standing {
        grants::standing(&env, &connector)
    }

    /// Reports why a grant was withdrawn.
    pub fn revocation(env: Env, connector: Symbol) -> Result<Revocation, Error> {
        env.storage()
            .persistent()
            .get(&Key::Revoked(connector))
            .ok_or(Error::UnknownGrant)
    }

    /// Reports every connector this registry knows about.
    pub fn connectors(env: Env) -> Vec<Symbol> {
        grants::connectors(&env)
    }

    /// Checks one call against a grant, and records what it consumed.
    ///
    /// The function stelfin calls before letting a connector propose anything.
    /// It is on chain rather than in the bot because that is what makes it
    /// checkable: a DAO can read this contract and know the limits its bot is
    /// working to, without taking the bot's word for them.
    ///
    /// Requires the admin's authorisation, so a stranger cannot burn a
    /// connector's daily allowance by calling this in a loop.
    pub fn authorise(
        env: Env,
        connector: Symbol,
        surface_digest: BytesN<32>,
        endpoint_hash: BytesN<32>,
        amount: i128,
    ) -> Result<(), Error> {
        grants::require_admin(&env)?;
        grants::authorise(&env, &connector, &surface_digest, &endpoint_hash, amount)
    }

    /// Reports what a connector may still move today.
    pub fn remaining(env: Env, connector: Symbol) -> Result<i128, Error> {
        grants::remaining(&env, &connector)
    }
}
