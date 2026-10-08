package treasuryabi

import (
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// The same three values the contract's own suite encodes, in
// contracts/dao-treasury/src/test.rs.
const (
	ada   = "GDKQYYPKNOHKK6W5JVT57WZDEWHQY5IQYLZHFNXS2326H7ROC5ZU5763"
	payee = "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
	token = "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA"
)

func policy() Policy {
	return Policy{
		QuorumBps: 5_000, ApprovalBps: 6_000,
		VotingPeriod: 259_200, Timelock: 86_400,
		SpendLimit: big.NewInt(2_500_000_000), SpendWindow: 86_400,
	}
}

func payment() Action {
	return Action{Kind: Pay, Payment: &Payment{
		Token: token, To: payee, Amount: big.NewInt(12_500_000_000),
	}}
}

func sevens() [32]byte {
	var memo [32]byte
	for i := range memo {
		memo[i] = 7
	}
	return memo
}

// golden reads the file the contract's suite writes: a name, a tab, and the
// value's XDR in hex.
func golden(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "dao-treasury", "testdata", "abi.tsv"))
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		name, value, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("golden line %q has no tab", line)
		}
		out[name] = value
	}
	return out
}

func must(t *testing.T) func(xdr.ScVal, error) xdr.ScVal {
	return func(v xdr.ScVal, err error) xdr.ScVal {
		t.Helper()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return v
	}
}

// What Go sends is, byte for byte, what the contract's own types encode to.
//
// The file is written by the Rust suite from the contract's real types, so
// this is not Go agreeing with itself. A field renamed in types.rs changes the
// file, and this fails until the encoding here is changed to match.
func TestGoEncodesWhatTheContractEncodes(t *testing.T) {
	want := golden(t)
	ok := must(t)

	ours := map[string]xdr.ScVal{
		"policy":                 ok(policy().ScVal()),
		"member":                 ok(Member{Address: ada, Weight: 2}.ScVal()),
		"members":                ok(Members([]Member{{ada, 2}, {payee, 1}})),
		"addresses":              ok(Addresses([]string{payee, ada})),
		"action_pay":             ok(payment().ScVal()),
		"action_set_policy":      ok(Action{Kind: SetPolicy, Policy: ptr(policy())}.ScVal()),
		"action_set_member":      ok(Action{Kind: SetMember, Member: &Member{payee, 0}}.ScVal()),
		"action_allow_recipient": ok(Action{Kind: AllowRecipient, Recipient: payee}.ScVal()),
		"action_deny_recipient":  ok(Action{Kind: DenyRecipient, Recipient: payee}.ScVal()),
		"action_revoke":          ok(Action{Kind: Revoke, Target: 7}.ScVal()),
		"memo":                   Memo(sevens()),
	}
	for name, value := range ours {
		raw, err := value.MarshalBinary()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := hex.EncodeToString(raw); got != want[name] {
			t.Errorf("%s:\n  go       %s\n  contract %s", name, got, want[name])
		}
	}

	// Everything the file holds is either encoded above or decoded below. A
	// value added to it and checked by neither would be a line that proves
	// nothing.
	decodedOnly := map[string]bool{"status_passed": true, "proposal": true}
	for name := range want {
		if _, encoded := ours[name]; !encoded && !decodedOnly[name] {
			t.Errorf("the golden file has %q and nothing here checks it", name)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func decode(t *testing.T, hexed string) xdr.ScVal {
	t.Helper()
	raw, err := hex.DecodeString(hexed)
	if err != nil {
		t.Fatalf("golden hex: %v", err)
	}
	var v xdr.ScVal
	if err := v.UnmarshalBinary(raw); err != nil {
		t.Fatalf("golden xdr: %v", err)
	}
	return v
}

// And what the contract returns is read back as what it was.
func TestGoReadsWhatTheContractReturns(t *testing.T) {
	want := golden(t)

	gotPolicy, err := AsPolicy(decode(t, want["policy"]))
	if err != nil || !reflect.DeepEqual(gotPolicy, policy()) {
		t.Errorf("policy: %+v, %v", gotPolicy, err)
	}

	status, err := AsStatus(decode(t, want["status_passed"]))
	if err != nil || status != Passed {
		t.Errorf("status: %q, %v", status, err)
	}

	addresses, err := AsAddresses(decode(t, want["addresses"]))
	if err != nil || !reflect.DeepEqual(addresses, []string{payee, ada}) {
		t.Errorf("addresses: %v, %v", addresses, err)
	}

	proposal, err := AsProposal(decode(t, want["proposal"]))
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	wantProposal := Proposal{
		ID: 3, Proposer: ada, Action: payment(), Memo: sevens(), Status: Passed,
		ClosesAt: 1_700_086_400, ExecutableAt: 1_700_090_000, Yes: 2, No: 1,
	}
	if !reflect.DeepEqual(proposal, wantProposal) {
		t.Errorf("proposal:\n  got  %+v\n  want %+v", proposal, wantProposal)
	}

	for name, kind := range map[string]string{
		"action_pay": Pay, "action_set_policy": SetPolicy, "action_set_member": SetMember,
		"action_allow_recipient": AllowRecipient, "action_deny_recipient": DenyRecipient,
		"action_revoke": Revoke,
	} {
		action, err := AsAction(decode(t, want[name]))
		if err != nil || action.Kind != kind {
			t.Errorf("%s: %+v, %v", name, action, err)
			continue
		}
		// Round trip: read, written again, the same bytes.
		again, err := action.ScVal()
		if err != nil {
			t.Errorf("%s: re-encode: %v", name, err)
			continue
		}
		raw, _ := again.MarshalBinary()
		if hex.EncodeToString(raw) != want[name] {
			t.Errorf("%s does not survive being read and written again", name)
		}
	}
}

// A value that is nearly the right shape is refused, not read as far as it
// goes.
func TestAValueOfTheWrongShapeIsRefused(t *testing.T) {
	want := golden(t)
	ok := must(t)

	cases := map[string]func() error{
		"a policy read as a proposal": func() error {
			_, err := AsProposal(decode(t, want["policy"]))
			return err
		},
		"a member read as a policy": func() error {
			_, err := AsPolicy(decode(t, want["member"]))
			return err
		},
		"a number read as an amount": func() error {
			_, err := AsAmount(U32(5))
			return err
		},
		"an amount read as a number": func() error {
			_, err := AsU32(ok(Amount(big.NewInt(5))))
			return err
		},
		"a status nobody defined": func() error {
			_, err := AsStatus(vec([]xdr.ScVal{symbol("Approved")}))
			return err
		},
		"an action nobody defined": func() error {
			_, err := AsAction(vec([]xdr.ScVal{symbol("Sweep"), U32(1)}))
			return err
		},
		"an action carrying two things": func() error {
			_, err := AsAction(vec([]xdr.ScVal{symbol(Revoke), U32(1), U32(2)}))
			return err
		},
		"a policy with a field too many": func() error {
			v := policy()
			encoded := ok(v.ScVal())
			entries := append(**encoded.Map, xdr.ScMapEntry{Key: symbol("zz_extra"), Val: U32(1)})
			p := &entries
			_, err := AsPolicy(xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &p})
			return err
		},
	}
	for name, try := range cases {
		if err := try(); !errors.Is(err, ErrShape) {
			t.Errorf("%s: %v, want ErrShape", name, err)
		}
	}

	// And on the way in.
	for name, action := range map[string]Action{
		"no such action":       {Kind: "Sweep"},
		"a payment of nothing": {Kind: Pay},
		"a bad recipient":      {Kind: AllowRecipient, Recipient: "not an address"},
		"an amount too wide": {Kind: Pay, Payment: &Payment{
			Token: token, To: payee, Amount: new(big.Int).Lsh(big.NewInt(1), 127),
		}},
	} {
		if _, err := action.ScVal(); !errors.Is(err, ErrShape) {
			t.Errorf("%s: %v, want ErrShape", name, err)
		}
	}
}

// Amounts are exact at every width, including the ones a 64-bit integer
// cannot hold and the negative ones a naive split gets wrong.
func TestAmountsSurviveTheRoundTripExactly(t *testing.T) {
	for _, text := range []string{
		"0", "1", "-1", "9223372036854775807", "9223372036854775808",
		"18446744073709551616", "-18446744073709551617",
		"170141183460469231731687303715884105727", "-170141183460469231731687303715884105728",
	} {
		n, _ := new(big.Int).SetString(text, 10)
		encoded, err := Amount(n)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		back, err := AsAmount(encoded)
		if err != nil || back.Cmp(n) != 0 {
			t.Errorf("%s came back as %v (%v)", text, back, err)
		}
	}
}

// The codes here are the contract's codes. Read out of the contract's source,
// so that one added there and not here is a failure rather than a number shown
// to somebody in place of a reason.
func TestTheErrorCodesAreTheContracts(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "dao-treasury", "src", "types.rs"))
	if err != nil {
		t.Fatalf("read types.rs: %v", err)
	}
	_, body, found := strings.Cut(string(source), "pub enum Error {")
	if !found {
		t.Fatal("types.rs has no Error enum")
	}
	body, _, _ = strings.Cut(body, "\n}")

	ours := map[string]Code{
		"NotInitialised": NotInitialised, "NotMember": NotMember,
		"UnknownProposal": UnknownProposal, "NotOpen": NotOpen, "AlreadyVoted": AlreadyVoted,
		"NotExecutable": NotExecutable, "RecipientNotAllowed": RecipientNotAllowed,
		"SpendLimitExceeded": SpendLimitExceeded, "InvalidPolicy": InvalidPolicy,
		"InvalidAmount": InvalidAmount, "WouldLockOut": WouldLockOut, "NotProposer": NotProposer,
		"InvalidMembers": InvalidMembers, "NothingToRevoke": NothingToRevoke, "Undecided": Undecided,
	}

	seen := 0
	for _, m := range regexp.MustCompile(`(?m)^\s*([A-Za-z]+) = (\d+),`).FindAllStringSubmatch(body, -1) {
		seen++
		n, _ := strconv.ParseUint(m[2], 10, 32)
		code, known := ours[m[1]]
		switch {
		case !known:
			t.Errorf("the contract refuses with %s = %d and this package has no name for it", m[1], n)
		case uint64(code) != n:
			t.Errorf("%s is %d in the contract and %d here", m[1], n, code)
		case !code.Known():
			t.Errorf("%s is not reported as known", m[1])
		}
	}
	if seen != len(ours) {
		t.Errorf("the contract defines %d refusals and this package %d", seen, len(ours))
	}
	if Code(3).Known() || Code(99).Known() {
		t.Error("a code the contract does not define is reported as known")
	}
}

// The version this package speaks is the one the contract answers.
func TestTheVersionIsTheContracts(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "dao-treasury", "src", "lib.rs"))
	if err != nil {
		t.Fatalf("read lib.rs: %v", err)
	}
	m := regexp.MustCompile(`const VERSION: u32 = (\d+);`).FindStringSubmatch(string(source))
	if m == nil {
		t.Fatal("lib.rs has no VERSION")
	}
	if m[1] != strconv.Itoa(Version) {
		t.Errorf("the contract is version %s and this package speaks %d", m[1], Version)
	}
}
