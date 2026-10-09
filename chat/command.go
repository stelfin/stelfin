package chat

// Role is an org-level permission level.
//
// Ordered, so a check is a comparison rather than a set membership test.
//
// The rule these encode, and the one worth stating loudly: a role decides who
// the bot will talk to. It does not decide who can spend. Whether an approval
// counts is decided on chain — by a treasury account's signer list and
// thresholds, or by a treasury contract's own list of voters — which the
// network enforces and this process cannot influence. Any change that lets a
// role here stand in for a signature is the bug that loses a treasury.
//
// So a link to sign is offered to anyone with a role, not only to approvers:
// the signature either counts or it does not, and no role makes it. What
// approver does mean is who a workspace has named as its deciders. When it
// creates a treasury held by a contract, they are who its first voters are —
// after which that list is the contract's, and changes by its voters' vote.
type Role uint8

const (
	// RoleNone is a person the org does not know.
	RoleNone Role = iota
	RoleObserver
	RoleProposer
	RoleApprover
	RoleAdmin
)

// AtLeast reports whether r meets the required level.
func (r Role) AtLeast(required Role) bool { return r >= required }

func (r Role) String() string {
	switch r {
	case RoleObserver:
		return "observer"
	case RoleProposer:
		return "proposer"
	case RoleApprover:
		return "approver"
	case RoleAdmin:
		return "admin"
	default:
		return "none"
	}
}

// OptionType is the type of a slash-command argument.
type OptionType string

const (
	OptString  OptionType = "string"
	OptInteger OptionType = "integer"
	OptUser    OptionType = "user"
	OptBool    OptionType = "bool"
)

// Option is one declared argument of a command.
type Option struct {
	Name        string
	Description string
	Type        OptionType
	Required    bool
}

// Command is declared once and registered with every transport at startup.
type Command struct {
	Name        string
	Description string
	Options     []Option
	// MinRole is the lowest org role that may invoke this command.
	//
	// A platform's own permission system is used as well, to hide commands
	// people cannot use, but it is never the check that counts: a Discord guild
	// admin can edit command permissions in server settings, and Telegram's
	// command scopes are only a hint to the autocomplete menu. The server-side
	// check against MinRole is the real gate on both platforms.
	MinRole Role
	// FreeText marks a command whose single argument is prose bound for the
	// decoder rather than a structured option.
	FreeText bool
}
