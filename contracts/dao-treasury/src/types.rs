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

/// A member and the weight their vote carries.
#[contracttype]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Member {
    pub address: Address,
    /// Zero, in a proposal, means remove them.
    pub weight: u32,
}

/// A payment out of the treasury.
#[contracttype]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Payment {
    /// The token to move and where to.
    pub token: Address,
    pub to: Address,
    pub amount: i128,
}

/// The one thing a proposal does.
///
/// Everything that moves money or changes who decides is in this list, and
/// nothing in this list can happen any other way. That is the whole of what
/// this version of the contract is for. The one before it had an admin who
/// could replace the policy, add themselves as a member, allow themselves as a
/// recipient and then spend — four calls, no vote, no timelock, under a comment
/// that said the admin could not move money. There is no admin now. A rule is
/// changed the way a payment is made: by the members, in the open, with time to
/// notice.
#[contracttype]
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Action {
    Pay(Payment),
    SetPolicy(Policy),
    /// Gives a member weight, changes it, or with zero removes them.
    SetMember(Member),
    /// Adds a recipient the treasury may pay.
    AllowRecipient(Address),
    /// Removes one. It makes the treasury able to do less, and so it does not
    /// wait out a timelock: see `restricts`.
    DenyRecipient(Address),
    /// Stops another proposal, by its number, before it is carried out.
    ///
    /// What the members do about something that passed and should not have.
    /// Like taking a recipient off the list, it only ever stops something, so
    /// it does not wait out a timelock either.
    Revoke(u32),
}

impl Action {
    /// Whether this action only ever narrows what the treasury can do.
    ///
    /// A timelock is the window in which the rest of a DAO notices that
    /// something which passed should not have, and what they do about it is one
    /// of these: take a recipient off the list, or stop the proposal itself. If
    /// that had to wait out a timelock of its own, it could never land before
    /// the thing it was meant to stop. So these take effect as soon as they are
    /// decided. They still need the votes.
    pub fn restricts(&self) -> bool {
        matches!(self, Action::DenyRecipient(_) | Action::Revoke(_))
    }
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
    pub action: Action,
    /// sha256 of the canonical description every member was shown.
    ///
    /// The text lives off chain, because a ledger is a bad place for prose and
    /// a good place for a commitment to it. What this proves is that the
    /// document people read and the proposal they voted on are the same
    /// document — not what that document said.
    pub memo: BytesN<32>,
    pub status: Status,
    /// When voting closes if nothing decides it sooner.
    pub closes_at: u64,
    /// The earliest it may be executed. Set when the proposal is opened, to the
    /// end of voting plus the timelock, and brought forward only when the
    /// outcome is settled early — never to less than a full timelock after the
    /// moment it was settled.
    pub executable_at: u64,
    /// Weight for and against, accumulated as votes arrive.
    pub yes: u32,
    pub no: u32,
}

/// Everything this contract can refuse.
///
/// Numbered explicitly and never renumbered: a client that maps a code to a
/// message would silently start showing the wrong one. The gaps are codes the
/// previous version used for an admin and a separate initialisation step, which
/// no longer exist; they are left unused rather than given to something else.
#[contracterror]
#[derive(Clone, Copy, Debug, Eq, PartialEq, PartialOrd, Ord)]
#[repr(u32)]
pub enum Error {
    NotInitialised = 2,
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
    /// Only whoever made a proposal may withdraw it.
    NotProposer = 14,
    /// A treasury was set up with no members, with one listed twice, or with a
    /// member given no weight.
    InvalidMembers = 15,
    /// The proposal to be stopped has already been carried out, rejected or
    /// withdrawn.
    NothingToRevoke = 16,
    /// Voting is still open and the votes not yet cast could change the result.
    Undecided = 17,
    /// The token would not make the payment: most often because the treasury
    /// does not hold enough of it.
    ///
    /// Reported as this contract's own refusal and not passed through as the
    /// token's. A token's error codes are numbered by the token, and one of
    /// them arriving from here would read as whichever of the codes above
    /// happened to share its number.
    TransferFailed = 18,
}
