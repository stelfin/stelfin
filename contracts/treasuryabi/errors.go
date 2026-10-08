package treasuryabi

// Code is one of the contract's refusals.
//
// The numbers are the contract's, in contracts/dao-treasury/src/types.rs, and
// are never renumbered there. A code this package does not know is reported as
// a number rather than guessed at.
type Code uint32

// The contract's refusals.
const (
	NotInitialised      Code = 2
	NotMember           Code = 4
	UnknownProposal     Code = 5
	NotOpen             Code = 6
	AlreadyVoted        Code = 7
	NotExecutable       Code = 8
	RecipientNotAllowed Code = 9
	SpendLimitExceeded  Code = 10
	InvalidPolicy       Code = 11
	InvalidAmount       Code = 12
	WouldLockOut        Code = 13
	NotProposer         Code = 14
	InvalidMembers      Code = 15
	NothingToRevoke     Code = 16
	Undecided           Code = 17
	TransferFailed      Code = 18
)

// Known reports whether this is a refusal the contract defines.
func (c Code) Known() bool {
	switch c {
	case NotInitialised, NotMember, UnknownProposal, NotOpen, AlreadyVoted,
		NotExecutable, RecipientNotAllowed, SpendLimitExceeded, InvalidPolicy,
		InvalidAmount, WouldLockOut, NotProposer, InvalidMembers, NothingToRevoke,
		Undecided, TransferFailed:
		return true
	default:
		return false
	}
}
