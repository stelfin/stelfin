//! What this contract says happened.
//!
//! Every change of state publishes one of these. A treasury's history should be
//! readable from the ledger by anybody, without asking the deployment that
//! built the transactions and without replaying them: who proposed what, who
//! voted which way, what was decided and when it was carried out.

use soroban_sdk::{contractevent, Address, BytesN};

use crate::types::Status;

/// A proposal was opened.
#[contractevent]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Proposed {
    #[topic]
    pub id: u32,
    pub proposer: Address,
    pub memo: BytesN<32>,
    pub closes_at: u64,
}

/// A member voted.
#[contractevent]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Voted {
    #[topic]
    pub id: u32,
    pub voter: Address,
    pub approve: bool,
    pub weight: u32,
}

/// A proposal stopped being open: it passed, was rejected, or was withdrawn.
#[contractevent]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Decided {
    #[topic]
    pub id: u32,
    pub status: Status,
    pub yes: u32,
    pub no: u32,
    /// The earliest it may be executed. Meaningful only for one that passed.
    pub executable_at: u64,
}

/// A passed proposal was carried out.
#[contractevent]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Executed {
    #[topic]
    pub id: u32,
}

/// A member paid out under the rolling limit, with no proposal.
#[contractevent]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Spent {
    #[topic]
    pub member: Address,
    pub token: Address,
    pub to: Address,
    pub amount: i128,
}
