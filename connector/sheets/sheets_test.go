package sheets_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/connector/sheets"
)

// tokens is a stand-in for the service-account exchange, so nothing here needs
// a key or a network.
type tokens struct {
	token string
	err   error
	orgs  []int64
}

func (t *tokens) Token(_ context.Context, org int64) (string, error) {
	t.orgs = append(t.orgs, org)
	return t.token, t.err
}

// serve stands up a fake Sheets API returning one body.
func serve(t *testing.T, body string, status int) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// reader points a Sheets reader at the fake server by rewriting the host.
func reader(t *testing.T, srv *httptest.Server, tk *tokens) *sheets.Sheets {
	t.Helper()
	client := &http.Client{Transport: rewrite{to: srv.URL, base: srv.Client().Transport}}
	s, err := sheets.New(42, tk, client)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

// rewrite sends every request to the test server, keeping the path.
type rewrite struct {
	to   string
	base http.RoundTripper
}

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(rw.to, "http://")
	base := rw.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// TestANumberKeepsItsDigits is the reason this package exists.
//
// Without UseNumber every numeric cell becomes a float64, and 1000.50 has been
// through two conversions before anybody looks at it.
func TestANumberKeepsItsDigits(t *testing.T) {
	srv, _ := serve(t, `{"range":"A1:B4","values":[
		["Name","Amount"],
		["Ada", 1000.50],
		["Bo", 0.0000001],
		["Cy", 500000000.0000001]
	]}`, http.StatusOK)

	got, err := reader(t, srv, &tokens{token: "t"}).Read(
		context.Background(), connector.Request{Selector: "sheet1!A1:B4", Limit: 100})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Rows) != 4 {
		t.Fatalf("%d rows", len(got.Rows))
	}

	for _, want := range []struct {
		row  int
		cell string
	}{
		{1, "1000.50"},
		{2, "0.0000001"},
		// The value that a float64 cannot hold and a string can.
		{3, "500000000.0000001"},
	} {
		if got := got.Rows[want.row][1].Unwrap(); got != want.cell {
			t.Errorf("row %d amount = %q, want %q", want.row, got, want.cell)
		}
	}
}

// TestTheValuesAreAskedForUnformatted: a formatted value carries the sheet's
// display settings, and reading a payment amount out of a locale's decimal
// comma is how a European sheet pays a thousandth of what it says.
func TestTheValuesAreAskedForUnformatted(t *testing.T) {
	srv, seen := serve(t, `{"values":[["1"]]}`, http.StatusOK)
	tk := &tokens{token: "secret-token"}

	if _, err := reader(t, srv, tk).Read(context.Background(),
		connector.Request{Selector: "abc123!Payroll!A1:C50"}); err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("%d requests", len(*seen))
	}
	r := (*seen)[0]
	if got := r.URL.Query().Get("valueRenderOption"); got != "UNFORMATTED_VALUE" {
		t.Errorf("valueRenderOption = %q", got)
	}
	if !strings.Contains(r.URL.Path, "abc123") {
		t.Errorf("path = %q", r.URL.Path)
	}
	// The range travels intact, including the sheet name's own separator.
	if !strings.Contains(r.URL.Path, "Payroll!A1:C50") {
		t.Errorf("the range did not survive: %q", r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("authorization = %q", got)
	}
	// The credential is fetched for the org the reader belongs to, not one
	// passed per call.
	if len(tk.orgs) != 1 || tk.orgs[0] != 42 {
		t.Errorf("tokens fetched for %v", tk.orgs)
	}
}

func TestCellsThatAreNotTextAreHandledDeliberately(t *testing.T) {
	srv, _ := serve(t, `{"values":[["Ada", true, false, null, "x"]]}`, http.StatusOK)

	got, err := reader(t, srv, &tokens{token: "t"}).Read(
		context.Background(), connector.Request{Selector: "s!A1:E1"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []string{"Ada", "TRUE", "FALSE", "", "x"}
	for i, w := range want {
		if got := got.Rows[0][i].Unwrap(); got != w {
			t.Errorf("cell %d = %q, want %q", i, got, w)
		}
	}
}

// TestANestedCellIsRefused: an error object the API returned in place of a
// value, rendered with %v, would produce Go syntax somebody might read as data.
func TestANestedCellIsRefused(t *testing.T) {
	srv, _ := serve(t, `{"values":[["Ada", {"error":"#REF!"}]]}`, http.StatusOK)

	_, err := reader(t, srv, &tokens{token: "t"}).Read(
		context.Background(), connector.Request{Selector: "s!A1:B1"})
	if !errors.Is(err, sheets.ErrUnreadableCell) {
		t.Fatalf("error = %v, want ErrUnreadableCell", err)
	}
	// And it says which cell, because a person has to go and look at it.
	if !strings.Contains(err.Error(), "row 1") || !strings.Contains(err.Error(), "column 2") {
		t.Errorf("the error does not locate the cell: %v", err)
	}
}

// TestTruncationIsReported: a payroll sheet cut at a hundred rows is a hundred
// people paid and the rest not, with nothing saying so.
func TestTruncationIsReported(t *testing.T) {
	srv, _ := serve(t, `{"values":[["1"],["2"],["3"],["4"]]}`, http.StatusOK)

	got, err := reader(t, srv, &tokens{token: "t"}).Read(
		context.Background(), connector.Request{Selector: "s!A1:A4", Limit: 2})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Rows) != 2 || !got.Truncated {
		t.Fatalf("%d rows, truncated = %v", len(got.Rows), got.Truncated)
	}

	// And a limit that fits does not claim truncation.
	got, err = reader(t, srv, &tokens{token: "t"}).Read(
		context.Background(), connector.Request{Selector: "s!A1:A4", Limit: 10})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Truncated {
		t.Error("a complete read reported itself truncated")
	}
}

// TestASelectorIsAnIdNotAUrl: accepting a URL means accepting a host, and a
// connector that can be pointed at a host is one whose credential can be sent
// to one.
func TestASelectorIsAnIdNotAUrl(t *testing.T) {
	srv, seen := serve(t, `{"values":[]}`, http.StatusOK)
	s := reader(t, srv, &tokens{token: "t"})

	for _, bad := range []string{
		"https://docs.google.com/spreadsheets/d/abc!A1:B2",
		"abc/../../etc!A1:B2",
		"abc",
		"!A1:B2",
		"abc!",
		"",
		"   ",
	} {
		if _, err := s.Read(context.Background(),
			connector.Request{Selector: bad}); !errors.Is(err, sheets.ErrBadSelector) {
			t.Errorf("%q: error = %v, want ErrBadSelector", bad, err)
		}
	}
	if len(*seen) != 0 {
		t.Errorf("%d requests were made for selectors that should not have been", len(*seen))
	}
}

func TestAnErrorFromGoogleIsNotQuotedBack(t *testing.T) {
	srv, _ := serve(t,
		`{"error":{"message":"The caller does not have permission","details":"secret"}}`,
		http.StatusForbidden)

	_, err := reader(t, srv, &tokens{token: "t"}).Read(
		context.Background(), connector.Request{Selector: "s!A1:B2"})
	if err == nil {
		t.Fatal("accepted")
	}
	// The status is useful; the body is Google's, can be long, and a permission
	// error's detail is not something to paste into a chat.
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error does not carry the status: %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error quotes the body back: %v", err)
	}
}

func TestAReaderIsAReader(t *testing.T) {
	srv, _ := serve(t, `{"values":[]}`, http.StatusOK)
	s := reader(t, srv, &tokens{token: "t"})

	if err := connector.CheckKind(s); err != nil {
		t.Fatalf("the Sheets reader is not a well-formed connector: %v", err)
	}
	d := s.Describe()
	if d.ID != sheets.ID || d.Kind != connector.KindReader {
		t.Fatalf("descriptor = %+v", d)
	}
	// It offers reading and nothing else, which is what a grant will pin.
	if len(d.Capabilities) != 1 || d.Capabilities[0] != sheets.CapabilityReadRange {
		t.Errorf("capabilities = %v", d.Capabilities)
	}
}

func TestAReaderNeedsAnOrgAndATokenSource(t *testing.T) {
	if _, err := sheets.New(0, &tokens{}, nil); err == nil {
		t.Error("a reader with no org was created")
	}
	if _, err := sheets.New(42, nil, nil); err == nil {
		t.Error("a reader with no token source was created")
	}
}
