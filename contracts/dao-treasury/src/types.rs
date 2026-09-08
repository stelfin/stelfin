use soroban_sdk::{contracterror, contracttype, Address, BytesN};

/// What a DAO's treasury is allowed to do without asking, and what it must ask
/// about.
///
/// Every field here is a limit rather than a permission. The contract's job is
/// to make some things impossible, not to make things possible — anything not
/// described below cannot be done through this contract at all.
#[contracttype]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Policy {
    /// Total weight that must vote before a proposal can pass at all.
    ///
    /// Expressed against total member weight in basis points, so a treasury can
    /// change its membership without recomputing a threshold. Quorum and
    /// approval are separate numbers because they answer different questions:
    /// how many turned up, and how many of those agreed.
    pub quorum_bps: u32,
    /// Share of the weight that voted which must be in favour.
    pub approval_bps: u32,
    /// How long voting stays open, in seconds.
    pub voting_period: u64,
    /// How long a passed proposal must wait before it can be executed.
    ///
    /// The only defence against a majority that has just been captured. Zero is
    /// permitted and is a deliberate choice a DAO makes about itself, not a
    /// default anyone should drift into.
    pub timelock: u64,
    /// How much may be spent without a proposal, per rolling window.
    ///
    /// Zero means nothing may be: every payment is a proposal. That is the
    /// default, and turning it up is how a treasury trades review for speed on
    /// small amounts.
    pub spend_limit: i128,
    /// The width of that window, in seconds.
    pub spend_window: u64,
}

/// A proposal's state. Stored rather than derived, because "passed" is a fact
/// about a moment — the membership can change afterwards, and a proposal that
/// passed does not un-pass when somebody leaves.
#[contracttype]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Status {
    Open,
    Passed,
    Executed,
    Rejected,
    Cancelled,
}

/// One thing a treasury has been asked to do.
#[contracttype]
#[derive(Clone, Debug)]
pub struct Proposal {
    pub id: u32,
    pub proposer: Address,
    /// The token to move and where to.
    pub token: Address,
    pub to: Address,
    pub amount: i128,
    /// sha256 of the canonical description every member was shown.
    ///
    /// The text lives off chain, because a ledger is a bad place for prose and
    /// a good place for a commitment to it. What this proves is that the
    /// document people read and the proposal they voted on are the same
    /// document — not what that document said.
    pub memo: BytesN<32>,
    pub status: Status,
    /// When voting closes, and the earliest it may be executed.
    pub closes_at: u64,
    pub executable_at: u64,
    /// Weight for and against, accumulated as votes arrive.
    pub yes: u32,
    pub no: u32,
}

/// Everything this contract can refuse.
///
/// Numbered explicitly and never renumbered: a client that maps a code to a
/// message would silently start showing the wrong one.
#[contracterror]
#[derive(Clone, Copy, Debug, Eq, PartialEq, PartialOrd, Ord)]
#[repr(u32)]
pub enum Error {
    AlreadyInitialised = 1,
    NotInitialised = 2,
    NotAdmin = 3,
    NotMember = 4,
    UnknownProposal = 5,
    /// Voting has closed, or the proposal is no longer open.
    NotOpen = 6,
    AlreadyVoted = 7,
    /// The proposal has not passed, or has not finished its timelock.
    NotExecutable = 8,
    /// The recipient is not on the allowlist.
    RecipientNotAllowed = 9,
    /// The amount would exceed what may be spent without a proposal.
    SpendLimitExceeded = 10,
    /// A policy that could never pass anything, or could pass everything.
    InvalidPolicy = 11,
    InvalidAmount = 12,
    /// Removing this member would leave the treasury unable to reach quorum.
    WouldLockOut = 13,
}
