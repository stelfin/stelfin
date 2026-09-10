// Package sheets reads a Google Sheet as a connector.
//
// The first connector, and the one the thesis was written about: a community's
// payroll, its grant list and its budget are in a spreadsheet, and the point of
// the product is that they should not have to be retyped.
//
// # Every cell comes back as text
//
// A spreadsheet holds numbers as IEEE doubles — that is Google's storage, not a
// choice available here — so a cell can already be imprecise before stelfin
// sees it. What this package can control is not making it worse, and it does
// that by never letting a number become a float64 in this process: the JSON
// decoder is put in UseNumber mode, so 1000.50 arrives as the four characters
// somebody typed rather than as a double that has been through two conversions.
//
// The values are requested unformatted for the same reason. A formatted value
// carries the sheet's display settings — currency symbols, thousands
// separators, a locale's decimal comma — and reading a payment amount out of a
// display setting is how a European sheet pays a thousandth of what it says.
package sheets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stelfin/stelfin/connector"
)

const (
	// ID is the connector id a grant is recorded against.
	ID = "sheets"

	// CapabilityReadRange is the one thing this offers.
	CapabilityReadRange = "read.range"

	// endpoint is Google's Sheets API. A constant rather than configuration:
	// a connector whose endpoint can be pointed elsewhere is a connector whose
	// credential can be sent elsewhere.
	endpoint = "https://sheets.googleapis.com/v4/spreadsheets/"
)

var (
	// ErrUnreadableCell reports a cell this package will not turn into text.
	ErrUnreadableCell = errors.New("sheets: a cell cannot be read as text")

	// ErrBadSelector reports a range this package will not request.
	ErrBadSelector = errors.New("sheets: that is not a spreadsheet and range")
)

// Tokens supplies a bearer token for the Sheets API.
//
// An interface so the service-account exchange lives outside this package and
// the reading can be tested without a network or a key. It is also where a
// per-org OAuth token will go when that lands, without this file changing.
type Tokens interface {
	Token(ctx context.Context, org int64) (string, error)
}

// Sheets reads ranges from Google Sheets.
type Sheets struct {
	client *http.Client
	tokens Tokens
	// org is whose credential to use. One reader per org rather than an org
	// argument on Read: a reader that took the org per call is one that can be
	// handed the wrong one.
	org int64
}

// New returns a reader for one org.
func New(org int64, tokens Tokens, client *http.Client) (*Sheets, error) {
	if org <= 0 {
		return nil, errors.New("sheets: a reader belongs to an org")
	}
	if tokens == nil {
		return nil, errors.New("sheets: a reader needs somewhere to get a token")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Sheets{client: client, tokens: tokens, org: org}, nil
}

// Describe is what a grant is checked against.
func (s *Sheets) Describe() connector.Descriptor {
	return connector.Descriptor{
		ID:           ID,
		Kind:         connector.KindReader,
		Capabilities: []string{CapabilityReadRange},
	}
}

// Read fetches one range.
//
// The selector is "<spreadsheet id>!<A1 range>", which is how a person refers
// to a range in the product's own vocabulary.
func (s *Sheets) Read(
	ctx context.Context, request connector.Request,
) (connector.Observation, error) {
	spreadsheet, cells, err := splitSelector(request.Selector)
	if err != nil {
		return connector.Observation{}, err
	}

	token, err := s.tokens.Token(ctx, s.org)
	if err != nil {
		return connector.Observation{}, fmt.Errorf("sheets: token: %w", err)
	}

	target := endpoint + url.PathEscape(spreadsheet) + "/values/" + url.PathEscape(cells) +
		"?valueRenderOption=UNFORMATTED_VALUE&dateTimeRenderOption=FORMATTED_STRING" +
		"&majorDimension=ROWS"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return connector.Observation{}, fmt.Errorf("sheets: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return connector.Observation{}, fmt.Errorf("sheets: fetch %s: %w", spreadsheet, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The body is not quoted back. It is Google's, it can be long, and a
		// permission error's detail is not something to paste into a chat.
		return connector.Observation{}, fmt.Errorf(
			"sheets: %s returned %s", spreadsheet, resp.Status)
	}

	// Bounded before it is read. A response this deployment cannot hold is not
	// something to discover by running out of memory.
	limited := io.LimitReader(resp.Body, maxResponseBytes)

	decoder := json.NewDecoder(limited)
	// The line this package exists for. Without it every numeric cell becomes a
	// float64 and 1000.50 has been through two conversions before anybody looks
	// at it.
	decoder.UseNumber()

	var body struct {
		Range  string  `json:"range"`
		Values [][]any `json:"values"`
	}
	if err := decoder.Decode(&body); err != nil {
		return connector.Observation{}, fmt.Errorf("sheets: decode %s: %w", spreadsheet, err)
	}

	return observationOf(body.Values, request.Limit)
}

// maxResponseBytes bounds one response.
const maxResponseBytes = 8 << 20

// observationOf turns Google's rows into text this system will handle.
func observationOf(values [][]any, limit int) (connector.Observation, error) {
	out := connector.Observation{ReadAt: time.Now().UTC()}

	if limit > 0 && len(values) > limit {
		// Reported rather than trimmed silently. A payroll sheet cut at a
		// hundred rows is a hundred people paid and the rest not, with nothing
		// saying so.
		out.Truncated = true
		values = values[:limit]
	}

	for i, row := range values {
		cells := make([]connector.Untrusted[string], 0, len(row))
		for j, cell := range row {
			text, err := cellText(cell)
			if err != nil {
				return connector.Observation{}, fmt.Errorf(
					"%w: row %d, column %d: %v", ErrUnreadableCell, i+1, j+1, err)
			}
			cells = append(cells, connector.Wrap(text))
		}
		out.Rows = append(out.Rows, cells)
	}
	return out, nil
}

// cellText renders one cell exactly.
func cellText(cell any) (string, error) {
	switch v := cell.(type) {
	case nil:
		// An empty cell. Google omits trailing empties entirely, so this is a
		// gap in the middle of a row and is ordinary.
		return "", nil
	case string:
		return v, nil
	case json.Number:
		// The digits Google sent, unchanged. Turning this into a float and back
		// is the whole failure this package is arranged to avoid.
		return v.String(), nil
	case bool:
		// A checkbox. Rendered rather than refused, because a reader may be
		// looking at a column of them — and it will never parse as an amount.
		if v {
			return "TRUE", nil
		}
		return "FALSE", nil
	default:
		// A nested object or array: an error the API returned in place of a
		// value, or a shape this version does not know. Refused rather than
		// rendered with %v, which would produce Go syntax somebody might read
		// as data.
		return "", fmt.Errorf("it is a %T", cell)
	}
}

// splitSelector reads "<spreadsheet id>!<A1 range>".
func splitSelector(selector string) (spreadsheet, cells string, err error) {
	spreadsheet, cells, found := strings.Cut(strings.TrimSpace(selector), "!")
	spreadsheet = strings.TrimSpace(spreadsheet)
	cells = strings.TrimSpace(cells)

	switch {
	case !found || spreadsheet == "" || cells == "":
		return "", "", fmt.Errorf(
			"%w: %q. It looks like 1a2B3c!Payroll!A1:C50", ErrBadSelector, selector)
	case strings.ContainsAny(spreadsheet, "/?#"):
		// A spreadsheet id, not a URL. Accepting a URL means accepting a host,
		// and a connector that can be pointed at a host is one whose credential
		// can be sent to one.
		return "", "", fmt.Errorf(
			"%w: %q is a URL, and this takes the spreadsheet's id", ErrBadSelector, spreadsheet)
	}
	return spreadsheet, cells, nil
}
