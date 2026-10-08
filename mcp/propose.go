package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

// The propose tier.
//
// A token of this tier can do one thing the read tier cannot: write a draft
// payroll for a person to review. Not a proposal — a draft. It moves no money,
// reserves no sequence number and asks no one to sign. A member with the
// proposer role has to look at it and decide to put it to the signers, and
// then the signers have to sign it.
//
// The draft says it came from an agent, by the label its token was minted
// with, wherever it is shown. The people reviewing it should know whether
// these rows came from a colleague's spreadsheet or from a program.
//
// What an agent writes is held to more than a member's sheet: every
// destination has to be an address. A saved name like "mum" belongs to the
// member who saved it, and an agent is nobody's mother.

// PaymentLine is one row an agent drafts, as text.
type PaymentLine struct {
	To     string `json:"to"`
	Amount string `json:"amount"`
	Memo   string `json:"memo,omitempty"`
}

// DraftSummary is what an agent is told about the draft it made.
type DraftSummary struct {
	ID    int64  `json:"draft"`
	Rows  int    `json:"rows"`
	Total string `json:"total"`
	Note  string `json:"note"`
}

// Drafter writes a draft on an agent's behalf. The api package implements it;
// it lives behind an interface so that this package does not depend on that
// one, which depends on a great deal.
//
// An error the agent should see — a row that cannot be paid — is returned as
// an *InvalidArgument. Anything else is reported to the agent as a failure and
// nothing more.
type Drafter interface {
	DraftForAgent(ctx context.Context, org ledger.OrgID, token int64, lines []PaymentLine, memo string) (DraftSummary, error)
}

// maxDraftLines bounds what one call can draft: one transaction holds a
// hundred operations, and a payroll is one transaction.
const maxDraftLines = 90

// WithDrafter turns on the propose tier's tool. Without one, a propose-tier
// token reaches exactly what a read-tier one does.
func (s *Server) WithDrafter(d Drafter) *Server {
	if d == nil {
		return s
	}
	s.drafter = d
	tool := draftTool()
	if _, exists := s.tools[tool.Name]; !exists {
		s.names = append(s.names, tool.Name)
	}
	s.tools[tool.Name] = tool
	return s
}

func draftTool() Tool {
	line := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"to", "amount"},
		"properties": map[string]any{
			"to": map[string]any{
				"type":        "string",
				"description": "A Stellar account address (G…). Saved names are not accepted.",
			},
			"amount": map[string]any{
				"type": "string",
				"description": "An exact decimal amount as a string, like \"1250.50\". " +
					"Numbers are refused: they would pass through a float.",
			},
			"memo": map[string]any{"type": "string"},
		},
	}
	return Tool{
		Name: "stelfin_draft_payments",
		Description: "Draft a payroll for a member of the workspace to review. This creates a draft " +
			"and nothing else: it is not a proposal, nobody is asked to sign it, and no money " +
			"moves unless a member proposes it and the treasury's signers sign it. " +
			"The draft is marked as written by this agent.",
		InputSchema: schema(map[string]any{
			"payments": map[string]any{
				"type": "array", "items": line, "minItems": 1, "maxItems": maxDraftLines,
			},
			"memo": map[string]any{
				"type":        "string",
				"description": "One note for the whole payroll, at most 28 bytes. It goes on chain.",
			},
		}, []string{"payments"}),
		Tier: store.TierPropose,
		Run:  draftPayments,
	}
}

func draftPayments(c callContext) (any, error) {
	if c.drafter == nil {
		return nil, invalidf("drafting is not available on this deployment")
	}

	// Read from the raw arguments, with numbers kept as their text, so that
	// an amount sent as a JSON number can be refused instead of rounded.
	var args struct {
		Payments []map[string]json.RawMessage `json:"payments"`
		Memo     string                       `json:"memo"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(c.raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return nil, invalidf("payments must be a list of {to, amount, memo}")
	}
	if len(args.Payments) == 0 {
		return nil, invalidf("there are no payments to draft")
	}
	if len(args.Payments) > maxDraftLines {
		return nil, invalidf("a draft holds at most %d payments", maxDraftLines)
	}

	lines := make([]PaymentLine, 0, len(args.Payments))
	for i, raw := range args.Payments {
		var line PaymentLine
		for key, value := range raw {
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				if key == "amount" {
					return nil, invalidf("payment %d: the amount must be a string, like \"%s\"",
						i+1, strings.Trim(string(value), " "))
				}
				return nil, invalidf("payment %d: %s must be a string", i+1, key)
			}
			switch key {
			case "to":
				line.To = text
			case "amount":
				line.Amount = text
			case "memo":
				line.Memo = text
			default:
				return nil, invalidf("payment %d: unknown field %q", i+1, key)
			}
		}
		if line.To == "" || line.Amount == "" {
			return nil, invalidf("payment %d needs to and amount", i+1)
		}
		lines = append(lines, line)
	}

	return c.drafter.DraftForAgent(c.ctx, c.org, c.token, lines, args.Memo)
}
