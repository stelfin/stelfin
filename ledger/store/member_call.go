package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/ledger"
)

var (
	// ErrNoMemberCall reports a hash with no call behind it here.
	ErrNoMemberCall = errors.New("store: no such call")
	// ErrCallNotYours reports a call issued to somebody else.
	ErrCallNotYours = errors.New("store: that call was issued to somebody else")
	// ErrCallSubmitted reports a call that has already been sent.
	ErrCallSubmitted = errors.New("store: that call has already been submitted")
	// ErrCallExpired reports a call past the time its envelope is valid for.
	ErrCallExpired = errors.New("store: that call has expired")
)

// MemberCall is one contract call, built for one member to sign.
type MemberCall struct {
	Hash     string
	Org      ledger.OrgID
	OwnerRef string
	Member   MemberID
	Treasury TreasuryID

	Source   string
	Contract string
	Function string
	XDR      string

	// Says is what stelfin told the member this call was.
	Says string
	// Note is a proposal's note, and Memo the commitment to it that the
	// contract stores. Memo is nil for a call that carries none.
	Note string
	Memo []byte

	ExpiresAt   time.Time
	CreatedAt   time.Time
	SubmittedAt *time.Time

	LandedHash   string
	LandedLedger int32
	Failed       bool
	// Proposal is the number the contract gave the proposal this call opened,
	// or zero.
	Proposal uint32
}

const memberCallColumns = `hash, org_id, owner_ref, member_id, treasury_id,
	source, contract, function, envelope_xdr, says, note, memo,
	expires_at, created_at, submitted_at,
	COALESCE(landed_hash, ''), COALESCE(landed_ledger, 0), failed, COALESCE(chain_proposal, 0)`

func scanMemberCall(row pgx.Row) (MemberCall, error) {
	var c MemberCall
	err := row.Scan(&c.Hash, &c.Org, &c.OwnerRef, &c.Member, &c.Treasury,
		&c.Source, &c.Contract, &c.Function, &c.XDR, &c.Says, &c.Note, &c.Memo,
		&c.ExpiresAt, &c.CreatedAt, &c.SubmittedAt,
		&c.LandedHash, &c.LandedLedger, &c.Failed, &c.Proposal)
	return c, err
}

// SaveMemberCall records a call that has been built and not yet signed.
//
// The same envelope asked for twice is the same row: a transaction's hash is
// its identity, so there is nothing a second row could mean.
func (s *Store) SaveMemberCall(ctx context.Context, c MemberCall) error {
	var memo any
	if len(c.Memo) > 0 {
		memo = c.Memo
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO member_calls (
			hash, org_id, owner_ref, member_id, treasury_id,
			source, contract, function, envelope_xdr, says, note, memo, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (hash) DO NOTHING`,
		c.Hash, int64(c.Org), c.OwnerRef, int64(c.Member), int64(c.Treasury),
		c.Source, c.Contract, c.Function, c.XDR, c.Says, c.Note, memo, c.ExpiresAt)
	if err != nil {
		return fmt.Errorf("store: save call %s: %w", c.Hash, err)
	}
	return nil
}

// MemberCall returns a call issued to this owner in this org.
//
// A call that exists and belongs to somebody else is reported exactly as one
// that does not exist. Telling the two apart would tell a stranger holding a
// hash that it names something.
func (s *Store) MemberCall(
	ctx context.Context, org ledger.OrgID, ownerRef, hash string,
) (MemberCall, error) {
	c, err := scanMemberCall(s.pool.QueryRow(ctx, `
		SELECT `+memberCallColumns+`
		  FROM member_calls
		 WHERE hash = $1 AND org_id = $2 AND owner_ref = $3`,
		hash, int64(org), ownerRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return MemberCall{}, fmt.Errorf("%w: %s", ErrNoMemberCall, hash)
	}
	if err != nil {
		return MemberCall{}, fmt.Errorf("store: load call %s: %w", hash, err)
	}
	return c, nil
}

// ClaimMemberCall marks a call as being submitted, once.
//
// A conditional update and not a read followed by a write, so that two
// submissions of the same envelope cannot both pass: exactly one changes the
// row, and the other finds nothing to claim and is told why.
func (s *Store) ClaimMemberCall(
	ctx context.Context, org ledger.OrgID, ownerRef, hash string,
) (MemberCall, error) {
	c, err := scanMemberCall(s.pool.QueryRow(ctx, `
		UPDATE member_calls
		   SET submitted_at = now()
		 WHERE hash = $1 AND org_id = $2 AND owner_ref = $3
		   AND submitted_at IS NULL AND expires_at > now()
		RETURNING `+memberCallColumns,
		hash, int64(org), ownerRef))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MemberCall{}, fmt.Errorf("store: claim call %s: %w", hash, err)
	}

	// Nothing was claimed. Say which of the reasons it was, scoped to the org
	// so that another tenant's hash looks like no hash at all.
	var (
		owner     string
		expiresAt time.Time
		submitted *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT owner_ref, expires_at, submitted_at
		  FROM member_calls WHERE hash = $1 AND org_id = $2`, hash, int64(org),
	).Scan(&owner, &expiresAt, &submitted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return MemberCall{}, fmt.Errorf("%w: %s", ErrNoMemberCall, hash)
	case err != nil:
		return MemberCall{}, fmt.Errorf("store: look up call %s: %w", hash, err)
	case owner != ownerRef:
		return MemberCall{}, fmt.Errorf("%w: %s", ErrCallNotYours, hash)
	case submitted != nil:
		return MemberCall{}, fmt.Errorf("%w: %s", ErrCallSubmitted, hash)
	default:
		return MemberCall{}, fmt.Errorf("%w: %s", ErrCallExpired, hash)
	}
}

// ReleaseMemberCall undoes a claim, for a call the network turned away before
// it reached a ledger.
//
// Only for that. A call whose outcome is not known keeps its claim, because
// releasing it would let the same envelope be paid for a second time on an
// outcome nobody established.
func (s *Store) ReleaseMemberCall(ctx context.Context, org ledger.OrgID, hash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE member_calls SET submitted_at = NULL
		 WHERE hash = $1 AND org_id = $2 AND landed_hash IS NULL`,
		hash, int64(org))
	if err != nil {
		return fmt.Errorf("store: release call %s: %w", hash, err)
	}
	return nil
}

// LandMemberCall records what became of a submitted call.
func (s *Store) LandMemberCall(
	ctx context.Context, org ledger.OrgID, hash, landedHash string,
	ledgerSeq int32, failed bool, proposal uint32,
) error {
	var chainProposal any
	if proposal > 0 {
		chainProposal = int32(proposal)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE member_calls
		   SET landed_hash = $3, landed_ledger = $4, failed = $5, chain_proposal = $6
		 WHERE hash = $1 AND org_id = $2`,
		hash, int64(org), landedHash, ledgerSeq, failed, chainProposal)
	if err != nil {
		return fmt.Errorf("store: record what became of call %s: %w", hash, err)
	}
	return nil
}

// ProposalNote is what a proposer wrote about a proposal, and the commitment
// to it that was sent to the contract.
type ProposalNote struct {
	Proposal uint32
	Note     string
	Memo     []byte
	By       MemberID
}

// ProposalNotes returns the notes this deployment holds for a treasury's
// proposals, by the number the contract gave each.
//
// Only for proposals opened through here. One made directly against the
// contract has a commitment on chain and no prose anywhere this can read, and
// is simply absent from the answer.
func (s *Store) ProposalNotes(
	ctx context.Context, org ledger.OrgID, treasury TreasuryID,
) (map[uint32]ProposalNote, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT chain_proposal, note, memo, member_id
		  FROM member_calls
		 WHERE org_id = $1 AND treasury_id = $2 AND chain_proposal IS NOT NULL`,
		int64(org), int64(treasury))
	if err != nil {
		return nil, fmt.Errorf("store: proposal notes: %w", err)
	}
	defer rows.Close()

	out := map[uint32]ProposalNote{}
	for rows.Next() {
		var (
			n  ProposalNote
			id int32
		)
		if err := rows.Scan(&id, &n.Note, &n.Memo, &n.By); err != nil {
			return nil, fmt.Errorf("store: scan proposal note: %w", err)
		}
		n.Proposal = uint32(id)
		out[n.Proposal] = n
	}
	return out, rows.Err()
}

// AddContractTreasury records a treasury held by a contract.
//
// Not a proof of control in the way a classic treasury's row is, because there
// is nothing to prove with a signature: a contract treasury is controlled by
// whoever its own member list says, and that is read from the contract every
// time it matters. What the row records is that this workspace works with this
// contract.
func (s *Store) AddContractTreasury(
	ctx context.Context, org ledger.OrgID, address, label string, by IdentityID,
) (Treasury, error) {
	return linkTreasuryTx(ctx, s.pool, LinkTreasuryParams{
		Org: org, Kind: TreasuryContract, Address: address, Label: label,
		Contract: address, VerifiedBy: by,
	})
}
