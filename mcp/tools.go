package mcp

import (
	"fmt"
	"time"

	"github.com/stelfin/stelfin/ledger/store"
)

// The read tier: what an agent may ask about a workspace.
//
// Four questions, all of them ones a member could answer by scrolling. Nothing
// here reaches another tenant, nothing here writes, and nothing here returns a
// number as a JSON number.
//
// What is absent is the design. There is no tool that signs, submits, executes
// a proposal, or edits the address book — and the last of those is the one that
// looks harmless. An agent that can add "payroll" pointing at an address it
// chose moves money the next time somebody pays payroll, without ever having
// touched a transaction.

func readTools() []Tool {
	return []Tool{
		{
			Name: "stelfin_balances",
			Description: "The workspace's balances, per account and asset. " +
				"Amounts are exact decimal strings, never numbers.",
			InputSchema: schema(nil, nil),
			Tier:        store.TierRead,
			Run:         balances,
		},
		{
			Name: "stelfin_treasuries",
			Description: "The accounts this workspace has proved control of, " +
				"and the signing weight each needs to spend.",
			InputSchema: schema(nil, nil),
			Tier:        store.TierRead,
			Run:         treasuries,
		},
		{
			Name: "stelfin_history",
			Description: "Recent movements in the workspace's ledger, newest first. " +
				"Amounts are exact decimal strings.",
			InputSchema: schema(map[string]any{
				"limit": map[string]any{
					"type":        "integer",
					"description": "How many rows to return, at most 100.",
					"minimum":     1,
					"maximum":     100,
				},
			}, nil),
			Tier: store.TierRead,
			Run:  history,
		},
		{
			Name: "stelfin_proposals",
			Description: "Recent treasury proposals and where each stands. " +
				"Reading one does not approve it.",
			InputSchema: schema(map[string]any{
				"limit": map[string]any{
					"type":        "integer",
					"description": "How many proposals to return, at most 50.",
					"minimum":     1,
					"maximum":     50,
				},
			}, nil),
			Tier: store.TierRead,
			Run:  proposals,
		},
	}
}

// schema builds a JSON Schema object for a tool's arguments.
func schema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
		// No arguments beyond those named. A schema that tolerated extras
		// would let an agent pass an org, a limit past the cap, or a field a
		// later version happens to read.
		"additionalProperties": false,
	}
}

// balanceRow is one balance, with the amount as text.
type balanceRow struct {
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Amount  string `json:"amount"`
}

func balances(c callContext) (any, error) {
	lines, err := c.store.TreasurySnapshot(c.ctx, c.org)
	if err != nil {
		return nil, err
	}
	out := make([]balanceRow, 0, len(lines))
	for _, line := range lines {
		out = append(out, balanceRow{
			Account: line.Name,
			Asset:   line.Code,
			// String, always. A JSON number is a float64 on the other side of
			// the wire whatever the sender meant, and a balance that survives
			// the round trip as 1000.5000001000001 is a wrong answer that looks
			// right.
			Amount: line.Balance.String(),
		})
	}
	return map[string]any{"balances": out}, nil
}

type treasuryRow struct {
	Address     string `json:"address"`
	Label       string `json:"label"`
	Kind        string `json:"kind"`
	SpendWeight string `json:"spend_weight"`
	ReadAt      string `json:"thresholds_read_at"`
}

func treasuries(c callContext) (any, error) {
	list, err := c.store.Treasuries(c.ctx, c.org)
	if err != nil {
		return nil, err
	}
	out := make([]treasuryRow, 0, len(list))
	for _, t := range list {
		out = append(out, treasuryRow{
			Address:     t.Address,
			Label:       t.Label,
			Kind:        t.Kind,
			SpendWeight: fmt.Sprint(t.Medium),
			// Dated, because it is a cached number. An agent reporting a
			// threshold as current when it is a week old is worse than one
			// reporting when it was read.
			ReadAt: t.RefreshedAt.UTC().Format(time.RFC3339),
		})
	}
	return map[string]any{"treasuries": out}, nil
}

type historyRow struct {
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Amount  string `json:"amount"`
	Ref     string `json:"transaction,omitempty"`
}

func history(c callContext) (any, error) {
	limit, err := intArg(c.args, "limit", 25, 1, 100)
	if err != nil {
		return nil, err
	}

	rows, _, err := c.store.History(c.ctx, c.org, store.HistoryFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]historyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, historyRow{
			At:      r.OccurredAt.UTC().Format(time.RFC3339),
			Kind:    string(r.Kind),
			Account: r.AccountName,
			Asset:   r.AssetCode,
			Amount:  r.Amount.String(),
			Ref:     r.ExternalRef,
		})
	}
	return map[string]any{"movements": out}, nil
}

type proposalRow struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func proposals(c callContext) (any, error) {
	limit, err := intArg(c.args, "limit", 20, 1, 50)
	if err != nil {
		return nil, err
	}

	list, err := c.store.Proposals(c.ctx, c.org, limit)
	if err != nil {
		return nil, err
	}
	out := make([]proposalRow, 0, len(list))
	for _, p := range list {
		row := proposalRow{
			// A string, like every other number here. An id read back as a
			// float loses precision above 2^53, and an agent quoting the wrong
			// proposal number is a person approving the wrong thing.
			ID:        fmt.Sprint(int64(p.ID)),
			Kind:      p.Kind,
			Status:    p.Status,
			CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
		}
		if p.Status == store.ProposalOpen {
			row.ExpiresAt = p.ExpiresAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return map[string]any{"proposals": out}, nil
}

// intArg reads a bounded integer argument.
//
// Bounds are enforced here rather than trusted from the schema. A schema is a
// description the client is asked to honour; this is the thing that happens.
func intArg(args map[string]any, name string, fallback, low, high int) (int, error) {
	raw, given := args[name]
	if !given || raw == nil {
		return fallback, nil
	}

	// JSON numbers arrive as float64 through a generic map. Accepted here
	// because a row count is small and exact in a float — unlike an amount,
	// which is why amounts never travel this way.
	value, ok := raw.(float64)
	if !ok || value != float64(int(value)) {
		return 0, invalidf("%s must be a whole number", name)
	}
	n := int(value)
	if n < low || n > high {
		return 0, invalidf("%s must be between %d and %d", name, low, high)
	}
	return n, nil
}
