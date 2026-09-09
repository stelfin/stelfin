use soroban_sdk::{contracterror, contracttype, Address, BytesN, Symbol, Vec};

/// What a DAO has allowed one connector to do, on chain, where anyone can check
/// it.
///
/// The whole point of putting this on a ledger is that a capability grant
/// becomes a public, timestamped, revocable fact rather than a row in stelfin's
/// database that stelfin could change. A DAO reading this contract can see
/// exactly what its bot is permitted to reach without trusting the bot.
///
/// # What is deliberately not here
///
/// No credential, no bearer token, no endpoint URL. Everything in a contract is
/// public — "private" storage does not exist — so anything secret written here
/// is published, not stored. The endpoint is committed to as a hash salted per
/// org, which proves the connector being called is the one that was registered
/// without telling the world where it is.
#[contracttype]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Grant {
    /// What the connector may do. Named capabilities rather than a bitmask:
    /// a mask read one version out of date grants the wrong thing silently.
    pub capabilities: Vec<Symbol>,
    /// sha256 of the tool surface this grant was issued against.
    ///
    /// A connector's own description of what it offers. Pinning it is what
    /// stops a server that was granted "read a spreadsheet" from later
    /// answering the same call with "and also transfer funds" — the digest
    /// changes, the pin fails, and the call is refused rather than obeyed.
    pub surface_digest: BytesN<32>,
    /// sha256(endpoint || org_salt).
    ///
    /// A commitment, not an address. It proves the endpoint being called is the
    /// one registered, and the salt stops anyone comparing two orgs' hashes to
    /// learn they use the same provider.
    pub endpoint_hash: BytesN<32>,
    /// The most one call may move, in stroops of whatever asset the caller
    /// names. Zero means the connector may propose nothing.
    pub per_call_limit: i128,
    /// The most it may move in a rolling day.
    pub daily_limit: i128,
    /// When the grant starts and stops being valid.
    pub not_before: u64,
    pub not_after: u64,
}

/// A grant's current standing.
#[contracttype]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Standing {
    /// Valid now.
    Active,
    /// Registered, but its window has not opened.
    NotYet,
    /// Its window has closed.
    Expired,
    /// Withdrawn, and recorded as withdrawn.
    Revoked,
    /// No grant exists.
    None,
}

/// A revocation, kept as a positive fact.
///
/// Deleting the grant would make "revoked" and "never granted" the same
/// observation, and they are not: one of them means somebody decided something.
/// An auditor asking why a connector stopped working deserves an answer.
#[contracttype]
#[derive(Clone, Debug)]
pub struct Revocation {
    pub at: u64,
    pub by: Address,
    pub reason: Symbol,
}

#[contracterror]
#[derive(Clone, Copy, Debug, Eq, PartialEq, PartialOrd, Ord)]
#[repr(u32)]
pub enum Error {
    AlreadyInitialised = 1,
    NotInitialised = 2,
    NotAdmin = 3,
    UnknownGrant = 4,
    AlreadyRevoked = 5,
    /// The window is empty, or the limits are inconsistent.
    InvalidGrant = 6,
    /// The grant exists but is not valid right now.
    NotActive = 7,
    /// The surface being called is not the one the grant was issued against.
    SurfaceChanged = 8,
    /// The endpoint being called is not the one registered.
    WrongEndpoint = 9,
    /// The amount exceeds a per-call or daily limit.
    OverLimit = 10,
}
