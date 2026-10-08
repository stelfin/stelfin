// Package webhook reads rows from a URL an org registered, as a connector.
//
// The most general connector there is, and for that reason the one with the
// least to say about what it will return. Sheets is Google's API in Google's
// shape; an MCP server at least declares its tools. This is a GET to an address
// somebody typed, answered by a program nobody here has seen. So the package is
// mostly a list of things it will not accept, and it is read tier only: a
// Webhook is a connector.Reader, and what it reads can be shown to a person and
// can do nothing else.
//
// # What it accepts
//
// One GET, answered with 200 and one of two content types.
//
// application/json, where the document is an array and every element is a row:
//
//   - An array of arrays of scalars. Each inner array is a row and each element
//     a cell. Columns is left empty: whether the first row is a heading is not
//     something this can know, so it is returned as the first row, as a sheet's
//     is.
//   - An array of flat objects. Each object is a row. Columns is the first
//     object's keys in the order the document gives them, and every later
//     object must have exactly those keys — in any order — or the document is
//     refused. A row with a key missing is not a row with an empty cell; it is
//     a row from a different table.
//
// The two are not mixed, and nothing is nested. A cell is a string, a number, a
// boolean or null, rendered as it is in sheets: a string as itself, a number as
// the digits that were sent, a boolean as TRUE or FALSE, null as empty.
//
// text/csv, read as RFC 4180. Every record is a row, the first included, and
// Columns is left empty for the reason above. Rows may differ in length.
//
// Anything else — another content type, a JSON object at the top, a nested
// value, text that is not UTF-8 — is refused whole. There is no partial read:
// one unreadable row is a source somebody is still editing.
//
// # Every number arrives as its text
//
// The JSON decoder runs in UseNumber mode and no cell is ever a float64 in this
// process. 1000.50 arrives as the seven characters the endpoint sent. Whether
// those characters are an amount is decided later, by stelfin's own parser, and
// it can only decide that correctly if it is shown what was sent.
//
// # The selector is not used
//
// Read refuses a request whose selector is not empty. The URL that was
// registered is the URL that is fetched, exactly: a selector that could append
// a path or a query would let whoever writes the request choose what the
// credential is used to fetch, and one that could name a host would let them
// choose where it is sent. An org that wants a second view registers a second
// webhook, and a DAO grants it separately.
package webhook

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/connector/egress"
)

// CapabilityReadRows is the one thing a webhook offers.
const CapabilityReadRows = "read.rows"

var (
	// ErrBadSelector reports a request that tried to say what to fetch.
	ErrBadSelector = errors.New("webhook: a webhook reads the address it was registered with and takes no selector")

	// ErrUnsupportedContent reports a response that is neither JSON nor CSV.
	ErrUnsupportedContent = errors.New("webhook: the endpoint answered with something that is neither JSON nor CSV")

	// ErrUnreadable reports a document that is not rows of plain cells: JSON
	// of the wrong shape, a nested value, rows that disagree about their
	// columns, malformed CSV.
	ErrUnreadable = errors.New("webhook: the response cannot be read as rows")

	// ErrTooLarge reports a row with too many cells or a cell that is too
	// long. A response with too many bytes is egress.ErrBodyTooLarge.
	//
	// An error rather than a cut. A cell shortened to fit is a different
	// value, and a row with its last columns dropped is a different row.
	ErrTooLarge = errors.New("webhook: the response is larger than this deployment will read")
)

// The bounds a webhook has when none are given.
const (
	// DefaultTimeout bounds one Read.
	DefaultTimeout = 15 * time.Second
	// DefaultMaxResponseBytes bounds the response body.
	DefaultMaxResponseBytes = 4 << 20
	// DefaultMaxColumns bounds the cells in one row.
	DefaultMaxColumns = 64
	// DefaultMaxCellBytes bounds one cell.
	DefaultMaxCellBytes = 4 << 10
)

// Config describes one registered webhook.
type Config struct {
	// ID is the connector id a grant is recorded against.
	ID string
	// URL is the address to fetch. It must satisfy HTTP.CheckURL.
	URL string
	// Credential is a bearer token for the endpoint, or empty for none. It is
	// copied.
	Credential []byte

	// HTTP is the client the request goes through.
	HTTP *egress.Client
	// Now is the clock. Nil means time.Now.
	Now connector.Clock

	// Timeout bounds one Read. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxResponseBytes bounds the body. Zero means DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// MaxColumns bounds the cells in a row. Zero means DefaultMaxColumns.
	MaxColumns int
	// MaxCellBytes bounds one cell. Zero means DefaultMaxCellBytes.
	MaxCellBytes int
}

// Webhook reads rows from one registered URL.
type Webhook struct {
	id   string
	url  string
	http *egress.Client
	now  connector.Clock
	// authorise adds the credential to a request. A function and not the
	// bytes, so that no way of printing a Webhook can print the credential.
	authorise func(*http.Request)

	timeout time.Duration
	bounds  bounds
	maxBody int64
}

// bounds are the limits on a document's shape.
type bounds struct {
	rows    int
	columns int
	cell    int
}

// New returns a reader for one registered URL.
func New(cfg Config) (*Webhook, error) {
	if cfg.ID == "" {
		return nil, errors.New("webhook: a webhook needs the connector id it is granted under")
	}
	if cfg.HTTP == nil {
		// Not defaulted. A default would be a way to fetch an org's URL
		// without the egress checks, by leaving a field out.
		return nil, errors.New("webhook: a webhook needs an egress client to make requests through")
	}
	if err := cfg.HTTP.CheckURL(cfg.URL); err != nil {
		return nil, fmt.Errorf("webhook: address: %w", err)
	}

	authorise := func(*http.Request) {}
	if len(cfg.Credential) > 0 {
		for _, b := range cfg.Credential {
			// A credential with a line break in it would be a second header.
			if b < 0x20 || b == 0x7f {
				return nil, errors.New("webhook: the credential contains a control character")
			}
		}
		header := "Bearer " + string(cfg.Credential)
		authorise = func(req *http.Request) { req.Header.Set("Authorization", header) }
	}

	w := &Webhook{
		id: cfg.ID, url: cfg.URL, http: cfg.HTTP, now: cfg.Now, authorise: authorise,
		timeout: cfg.Timeout,
		maxBody: cfg.MaxResponseBytes,
		bounds:  bounds{columns: cfg.MaxColumns, cell: cfg.MaxCellBytes},
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.timeout <= 0 {
		w.timeout = DefaultTimeout
	}
	if w.maxBody <= 0 {
		w.maxBody = DefaultMaxResponseBytes
	}
	if w.bounds.columns <= 0 {
		w.bounds.columns = DefaultMaxColumns
	}
	if w.bounds.cell <= 0 {
		w.bounds.cell = DefaultMaxCellBytes
	}
	return w, nil
}

// Describe is what a grant is checked against.
func (w *Webhook) Describe() connector.Descriptor {
	return connector.Descriptor{
		ID:           w.id,
		Kind:         connector.KindReader,
		Capabilities: []string{CapabilityReadRows},
	}
}

// Read fetches the registered URL and returns its rows.
func (w *Webhook) Read(
	ctx context.Context, request connector.Request,
) (connector.Observation, error) {
	if strings.TrimSpace(request.Selector) != "" {
		return connector.Observation{}, ErrBadSelector
	}

	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.url, nil)
	if err != nil {
		// Not wrapped: the error quotes the URL, which may hold a secret.
		return connector.Observation{}, errors.New("webhook: the request could not be built")
	}
	req.Header.Set("Accept", "application/json, text/csv")
	w.authorise(req)

	resp, err := w.http.Do(req)
	if err != nil {
		return connector.Observation{}, fmt.Errorf("webhook: %s: %w", w.id, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The number, and neither the reason phrase nor the body. Both are
		// the endpoint's text, and this error is on its way to a chat.
		return connector.Observation{}, fmt.Errorf(
			"webhook: %s answered with HTTP %d", w.id, resp.StatusCode)
	}

	kind, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (kind != "application/json" && kind != "text/csv") {
		// Decided from the header and before the body is read. Guessing a
		// format from the bytes is how an HTML error page becomes one row of
		// one cell.
		return connector.Observation{}, ErrUnsupportedContent
	}
	if charset := params["charset"]; charset != "" && !strings.EqualFold(charset, "utf-8") {
		return connector.Observation{}, fmt.Errorf("%w: it is not UTF-8", ErrUnsupportedContent)
	}

	body, err := egress.ReadBody(resp.Body, w.maxBody)
	if err != nil {
		return connector.Observation{}, fmt.Errorf("webhook: %s: %w", w.id, err)
	}
	if !utf8.Valid(body) {
		// Refused rather than repaired. A decoder that replaces a bad byte
		// with U+FFFD has changed a cell, and nothing downstream can tell.
		return connector.Observation{}, fmt.Errorf("%w: it is not UTF-8", ErrUnreadable)
	}

	limits := w.bounds
	limits.rows = request.Limit
	if limits.rows <= 0 || limits.rows > connector.MaxRows {
		limits.rows = connector.MaxRows
	}

	var out connector.Observation
	if kind == "text/csv" {
		out, err = readCSV(body, limits)
	} else {
		out, err = readJSON(body, limits)
	}
	if err != nil {
		return connector.Observation{}, err
	}
	out.ReadAt = w.now().UTC()
	return out, nil
}

// readJSON reads an array of rows.
//
// Walked token by token rather than decoded into a value, for two reasons. The
// order of an object's keys is the order of its columns, and a map does not
// keep it. And rows past the limit are still checked for shape without being
// held, so a document is either well-formed to the end or refused — while a
// response that is all row and no content cannot make this allocate in
// proportion to it.
func readJSON(body []byte, limits bounds) (connector.Observation, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	// The line that keeps a float away from an amount. Without it 1000.50 is
	// 1000.5 by the time anybody looks, and 500000000.0000001 is something
	// else entirely.
	decoder.UseNumber()

	if err := expect(decoder, '['); err != nil {
		return connector.Observation{}, fmt.Errorf(
			"%w: it is not a JSON array of rows", ErrUnreadable)
	}

	var (
		out     connector.Observation
		objects bool
		columns map[string]int
		line    int
	)
	for decoder.More() {
		line++
		opening, err := decoder.Token()
		if err != nil {
			return connector.Observation{}, malformed(err)
		}
		keep := len(out.Rows) < limits.rows
		if !keep {
			// Reported rather than trimmed silently, as everywhere a
			// connector cuts something short.
			out.Truncated = true
		}

		var row []connector.Untrusted[string]
		switch opening {
		case json.Delim('['):
			if line > 1 && objects {
				return connector.Observation{}, fmt.Errorf(
					"%w: row %d is an array and the rows before it are objects", ErrUnreadable, line)
			}
			row, err = readArrayRow(decoder, line, limits)

		case json.Delim('{'):
			if line > 1 && !objects {
				return connector.Observation{}, fmt.Errorf(
					"%w: row %d is an object and the rows before it are arrays", ErrUnreadable, line)
			}
			objects = true
			if columns == nil {
				out.Columns, row, err = readFirstObject(decoder, limits)
				columns = make(map[string]int, len(out.Columns))
				for i, name := range out.Columns {
					columns[name.Unwrap()] = i
				}
			} else {
				row, err = readObjectRow(decoder, line, columns, limits)
			}

		default:
			return connector.Observation{}, fmt.Errorf(
				"%w: row %d is neither an array nor an object", ErrUnreadable, line)
		}
		if err != nil {
			return connector.Observation{}, err
		}
		if keep {
			out.Rows = append(out.Rows, row)
		}
	}

	if err := expect(decoder, ']'); err != nil {
		return connector.Observation{}, malformed(err)
	}
	// One document. Anything after it is either a second answer or the tail
	// of something that was never meant to be parsed.
	if _, err := decoder.Token(); err != io.EOF {
		return connector.Observation{}, fmt.Errorf(
			"%w: there is something after the array", ErrUnreadable)
	}
	return out, nil
}

func expect(decoder *json.Decoder, delim rune) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim(delim) {
		return fmt.Errorf("unexpected %v", token)
	}
	return nil
}

// malformed reports JSON that does not parse, without quoting any of it.
func malformed(error) error {
	return fmt.Errorf("%w: it is not well-formed JSON", ErrUnreadable)
}

// readArrayRow reads the cells of one array row, after its opening bracket.
func readArrayRow(
	decoder *json.Decoder, line int, limits bounds,
) ([]connector.Untrusted[string], error) {
	var row []connector.Untrusted[string]
	for decoder.More() {
		if len(row) == limits.columns {
			return nil, fmt.Errorf("%w: row %d has more than %d cells",
				ErrTooLarge, line, limits.columns)
		}
		text, err := readCell(decoder, line, len(row)+1, limits)
		if err != nil {
			return nil, err
		}
		row = append(row, connector.Wrap(text))
	}
	if err := expect(decoder, ']'); err != nil {
		return nil, malformed(err)
	}
	return row, nil
}

// readFirstObject reads the object that names the columns.
func readFirstObject(
	decoder *json.Decoder, limits bounds,
) (columns, row []connector.Untrusted[string], err error) {
	seen := map[string]bool{}
	for decoder.More() {
		if len(row) == limits.columns {
			return nil, nil, fmt.Errorf("%w: row 1 has more than %d cells",
				ErrTooLarge, limits.columns)
		}
		key, err := readKey(decoder, 1, limits)
		if err != nil {
			return nil, nil, err
		}
		// A JSON object may repeat a key, and parsers disagree about which
		// value wins. A row that means different things to different readers
		// is refused instead.
		if seen[key] {
			return nil, nil, fmt.Errorf("%w: row 1 repeats a key", ErrUnreadable)
		}
		seen[key] = true

		text, err := readCell(decoder, 1, len(row)+1, limits)
		if err != nil {
			return nil, nil, err
		}
		columns = append(columns, connector.Wrap(key))
		row = append(row, connector.Wrap(text))
	}
	if err := expect(decoder, '}'); err != nil {
		return nil, nil, malformed(err)
	}
	return columns, row, nil
}

// readObjectRow reads a later object and lays it out under the first's columns.
func readObjectRow(
	decoder *json.Decoder, line int, columns map[string]int, limits bounds,
) ([]connector.Untrusted[string], error) {
	row := make([]connector.Untrusted[string], len(columns))
	filled := make([]bool, len(columns))
	count := 0

	for decoder.More() {
		key, err := readKey(decoder, line, limits)
		if err != nil {
			return nil, err
		}
		index, known := columns[key]
		if !known {
			// Refused rather than dropped. An extra key is a column the first
			// row did not have, and discarding it is deciding on the
			// reader's behalf that it did not matter.
			return nil, fmt.Errorf("%w: row %d has a key the first row does not",
				ErrUnreadable, line)
		}
		if filled[index] {
			return nil, fmt.Errorf("%w: row %d repeats a key", ErrUnreadable, line)
		}

		text, err := readCell(decoder, line, index+1, limits)
		if err != nil {
			return nil, err
		}
		row[index] = connector.Wrap(text)
		filled[index] = true
		count++
	}
	if err := expect(decoder, '}'); err != nil {
		return nil, malformed(err)
	}
	if count != len(columns) {
		return nil, fmt.Errorf("%w: row %d is missing a key the first row has",
			ErrUnreadable, line)
	}
	return row, nil
}

func readKey(decoder *json.Decoder, line int, limits bounds) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", malformed(err)
	}
	key, isString := token.(string)
	if !isString {
		return "", malformed(nil)
	}
	if len(key) > limits.cell {
		return "", fmt.Errorf("%w: row %d has a key longer than %d bytes",
			ErrTooLarge, line, limits.cell)
	}
	return key, nil
}

// readCell renders one scalar exactly, as sheets renders a cell.
func readCell(decoder *json.Decoder, line, column int, limits bounds) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", malformed(err)
	}

	var text string
	switch v := token.(type) {
	case nil:
		text = ""
	case string:
		text = v
	case json.Number:
		// The digits the endpoint sent, unchanged.
		text = v.String()
	case bool:
		text = "FALSE"
		if v {
			text = "TRUE"
		}
	default:
		// A nested array or object. Refused rather than flattened or
		// rendered: there is no one obvious text for it, and whichever was
		// chosen would be this package inventing a cell.
		return "", fmt.Errorf("%w: row %d, column %d is not a plain value",
			ErrUnreadable, line, column)
	}
	if len(text) > limits.cell {
		return "", fmt.Errorf("%w: row %d, column %d is longer than %d bytes",
			ErrTooLarge, line, column, limits.cell)
	}
	return text, nil
}

// readCSV reads RFC 4180 records as rows.
func readCSV(body []byte, limits bounds) (connector.Observation, error) {
	// A byte-order mark is not part of the first cell, though a spreadsheet
	// export will put one there.
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))

	reader := csv.NewReader(bytes.NewReader(body))
	// Rows may differ in length, as a sheet's do. What a short row means is
	// for whoever resolves it; inventing empty cells to square it off is not
	// this package's call.
	reader.FieldsPerRecord = -1
	// Strict quoting. The lenient mode exists to accept files that are not
	// CSV, by guessing where a field ends.
	reader.LazyQuotes = false

	var out connector.Observation
	for line := 1; ; line++ {
		record, err := reader.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			// encoding/csv quotes nothing from the input in its errors, but
			// the position it reports is all a person needs and this says it
			// in one voice.
			return connector.Observation{}, fmt.Errorf(
				"%w: record %d is not well-formed CSV", ErrUnreadable, line)
		}
		if len(record) > limits.columns {
			return connector.Observation{}, fmt.Errorf("%w: row %d has more than %d cells",
				ErrTooLarge, line, limits.columns)
		}
		for column, cell := range record {
			if len(cell) > limits.cell {
				return connector.Observation{}, fmt.Errorf(
					"%w: row %d, column %d is longer than %d bytes",
					ErrTooLarge, line, column+1, limits.cell)
			}
		}

		if len(out.Rows) == limits.rows {
			out.Truncated = true
			continue
		}
		row := make([]connector.Untrusted[string], len(record))
		for i, cell := range record {
			row[i] = connector.Wrap(cell)
		}
		out.Rows = append(out.Rows, row)
	}
}
