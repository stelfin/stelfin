//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/contracts"
	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/settlement"
)

// TestContractTreasuryEndToEnd walks a treasury held by a contract, on
// testnet, from nothing to money at its destination.
//
// The other test in this package is a treasury that is an account: one
// envelope, signed by enough of its signers. This one is a contract its
// members vote in, and nothing about it is signed that way. Each member makes
// their own call from their own wallet; the contract — the code in
// contracts/dao-treasury, uploaded to testnet and pinned in this repository —
// counts the votes, holds the money and enforces the wait.
//
// What it proves, against the real network:
//
//   - a treasury is created by the server with a workspace's approvers as its
//     voters, and the server that created it has no say in it afterwards;
//   - a member moves their own money in, by a call they sign;
//   - a payment is a proposal, which one member cannot carry out, which passes
//     when a second votes for it, and which still waits out its timelock;
//   - once that has run, it is carried out by a call nobody signs but the
//     relay, and the money is at its destination;
//   - a second payment that has passed is stopped by the voters before it is
//     carried out, and then cannot be;
//   - a member's own call is paid for by the operator, so a member needs no
//     XLM of their own to take part beyond what keeps their account open.
//
// The browser's part is played the way the other test plays it: by calling
// the JSON endpoints the page calls, and signing with the member's key.
func TestContractTreasuryEndToEnd(t *testing.T) {
	horizon := &horizonclient.Client{HorizonURL: horizonURL, HTTP: &http.Client{Timeout: 60 * time.Second}}

	if _, err := contracts.Lookup(contracts.DAOTreasury, contracts.Testnet); err != nil {
		t.Fatalf("the treasury contract is not pinned for testnet: %v", err)
	}

	// ---- The world outside stelfin. Unlike the signers of a classic
	// treasury, a contract's voters need accounts: a call has to come from
	// one.

	operator := funded(t, horizon, "operator")
	ada := funded(t, horizon, "ada")
	ben := funded(t, horizon, "ben")
	carol := funded(t, horizon, "carol") // has a wallet, and no vote
	payee := funded(t, horizon, "payee")
	webAuth := keypair.MustRandom()
	// Not used for anything here but required by configuration: the asset the
	// deployment is denominated in. The treasury in this test holds XLM.
	issuer := keypair.MustRandom()

	db, err := pgtest.Start(freePort(t), func(string) error { return nil })
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Stop() })

	telegram := newFakeTelegram(t)
	port := freePort(t)
	base := fmt.Sprintf("http://localhost:%d", port)

	// Where the books begin. A contract's money moves in token events, and
	// the server reads those from a ledger it has to be given: here, the one
	// the network is on as this test starts, so everything below is after it.
	root, err := horizon.Root()
	if err != nil {
		t.Fatalf("read the network's latest ledger: %v", err)
	}

	startServer(t, base, map[string]string{
		"STELFIN_HTTP_ADDR":               fmt.Sprintf("127.0.0.1:%d", port),
		"STELFIN_BASE_URL":                base,
		"STELFIN_DATABASE_URL":            db.DSN,
		"STELFIN_NETWORK":                 "testnet",
		"STELFIN_HORIZON_URL":             horizonURL,
		"STELFIN_SOROBAN_RPC_URL":         sorobanURL,
		"STELFIN_SOROBAN_START_LEDGER":    fmt.Sprint(root.HorizonSequence),
		"STELFIN_TREASURY_SEED":           operator.Seed(),
		"STELFIN_WEBAUTH_SEED":            webAuth.Seed(),
		"STELFIN_ASSET_CODE":              "USDC",
		"STELFIN_ASSET_ISSUER":            issuer.Address(),
		"STELFIN_CONFIRM_TOKEN_SECRET":    confirmSecret,
		"STELFIN_TELEGRAM_BOT_TOKEN":      botToken,
		"STELFIN_TELEGRAM_WEBHOOK_SECRET": webhookSecret,
		"STELFIN_TELEGRAM_API_URL":        telegram.URL,
		"STELFIN_SHUTDOWN_GRACE":          "5s",
	})

	// The commands a contract treasury needs are offered, because this
	// deployment has an RPC and the code is on this network.
	published := telegram.published(t)
	for _, want := range []string{"create_treasury", "deposit", "reject", "stop", "allow", "voter", "rules", "spend"} {
		if !contains(published, want) {
			t.Errorf("/%s is not in the menu: %v", want, published)
		}
	}

	chat := &group{t: t, base: base, telegram: telegram}
	people := map[int64]*keypair.Full{adaID: ada, benID: ben, carolID: carol}
	names := map[int64]string{adaID: "ada", benID: "ben", carolID: "carol"}

	// ---- A workspace, and three people with wallets in it.

	mustContain(t, chat.ask(adaID, "ada", "/setup Voters DAO"), "Voters DAO")
	for _, id := range []int64{adaID, benID, carolID} {
		linkWallet(t, chat, base, id, names[id], people[id])
	}

	// Ben may approve; Carol is only somebody the bot talks to. That is what
	// decides who votes in what is about to be created.
	grant(t, chat, benID, "ben", "approver")
	grant(t, chat, carolID, "carol", "observer")

	// ---- The treasury. Ten minutes of voting and a forty-second timelock:
	// long enough to be refused inside it, short enough to wait out.

	const timelock = 40 * time.Second
	reply := chat.ask(adaID, "ada", fmt.Sprintf(
		"/create-treasury ops voting:10m timelock:%ds recipients:%s", int(timelock.Seconds()), payee.Address()))
	mustContain(t, reply, "ops is created, and held by a contract at")
	mustContain(t, reply, "timelock 40s")
	mustContain(t, reply, "Voters (2, total weight 2)")
	mustContain(t, reply, ada.Address())
	mustContain(t, reply, ben.Address())
	if strings.Contains(reply, carol.Address()) {
		t.Fatalf("somebody who may not approve was made a voter:\n%s", reply)
	}
	mustContain(t, reply, "May pay:")
	mustContain(t, reply, payee.Address())
	found := regexp.MustCompile(`\bC[A-Z2-7]{55}\b`).FindString(reply)
	if found == "" {
		t.Fatalf("the reply does not say where the treasury is:\n%s", reply)
	}
	treasury := found
	t.Logf("treasury contract %s", treasury)

	mustContain(t, chat.ask(carolID, "carol", "/treasuries"), treasury)
	rules := chat.ask(carolID, "carol", "/rules")
	mustContain(t, rules, "quorum 50%, approval 60%, voting open 10m, timelock 40s")
	mustContain(t, rules, "Read from the contract just now")

	// ---- Money in. Ada moves forty of her own XLM, by a call she signs.

	reply = chat.ask(adaID, "ada", "/deposit 40 XLM")
	mustContain(t, reply, "from your wallet "+ada.Address())
	deposit := memberCall(t, base, linkToken(t, reply, base, "call"))
	if deposit.Member != ada.Address() || deposit.Treasury != treasury {
		t.Fatalf("the deposit is from %s into %s", deposit.Member, deposit.Treasury)
	}
	mustContain(t, deposit.Canonical, "transfer")
	sent := deposit.send(t, base, ada)
	if sent.Function != "transfer" {
		t.Fatalf("sent %+v", sent)
	}
	mustContain(t, chat.ask(carolID, "carol", "/balance"), "40.00 XLM")

	// ---- A proposal. One member cannot make it happen.

	before := native(t, horizon, payee.Address())

	// Somebody the contract may not pay is refused before anybody signs.
	mustContain(t, chat.ask(adaID, "ada", "/pay 5 to "+carol.Address()+" asset:XLM"), "isn't one ops may pay")

	reply = chat.ask(adaID, "ada", "/pay 12 to "+payee.Address()+" asset:XLM for the audit")
	mustContain(t, reply, "pay 12.00 XLM to "+payee.Address())
	mustContain(t, reply, "only a proposal")
	propose := memberCall(t, base, linkToken(t, reply, base, "call"))
	if propose.Note != "for the audit" || !strings.Contains(propose.Says, "pay 12.00 XLM") {
		t.Fatalf("the proposal is described as %q with note %q", propose.Says, propose.Note)
	}

	// The link is hers. Somebody else's signature on her call is turned away
	// by the network, and the call is still there for her to sign.
	propose.refuse(t, base, ben, http.StatusBadRequest)
	opened := propose.send(t, base, ada)
	if opened.Proposal != 1 {
		t.Fatalf("the first proposal is numbered %d", opened.Proposal)
	}
	// Once. The same envelope again is not sent a second time.
	propose.refuse(t, base, ada, http.StatusConflict)

	listing := chat.ask(carolID, "carol", "/proposals")
	mustContain(t, listing, "1 · pay 12.00 XLM to "+payee.Address())
	mustContain(t, listing, "Note: for the audit")
	mustContain(t, listing, "weight 0 for, 0 against")

	// Carol has a wallet and no vote. The contract says so, before she signs.
	mustContain(t, chat.ask(carolID, "carol", "/approve 1"), "has no vote in ops")

	reply = chat.ask(adaID, "ada", "/approve 1")
	mustContain(t, reply, "Your vote for proposal 1")
	mustContain(t, reply, "Its proposer's note: for the audit")
	memberCall(t, base, linkToken(t, reply, base, "call")).send(t, base, ada)

	// One of two voters. Nothing can be carried out, and nothing was.
	mustContain(t, chat.ask(benID, "ben", "/execute 1"), "can't be carried out yet")
	mustContain(t, chat.ask(adaID, "ada", "/approve 1"), "already voted")
	// Ben did not make it, and there is nobody above the voters to ask.
	mustContain(t, chat.ask(benID, "ben", "/cancel 1"), "only that wallet can withdraw it")

	// ---- The second vote settles it, and the timelock starts.

	reply = chat.ask(benID, "ben", "/approve 1")
	vote := memberCall(t, base, linkToken(t, reply, base, "call"))
	mustContain(t, vote.Says, "Vote for proposal 1")
	vote.send(t, base, ben)
	passedAt := time.Now()

	mustContain(t, chat.ask(carolID, "carol", "/proposals"), "It passed, and can be carried out from")
	mustContain(t, chat.ask(benID, "ben", "/execute 1"), "its timelock runs until")
	if got := native(t, horizon, payee.Address()); got != before {
		t.Fatalf("the payee holds %s inside the timelock; it held %s before", got, before)
	}
	// A vote is not taken once the outcome is settled.
	mustContain(t, chat.ask(benID, "ben", "/reject 1"), "not taking votes any more")

	// ---- A second payment, which the voters stop while the first waits.

	reply = chat.ask(benID, "ben", "/pay 3 to "+payee.Address()+" asset:XLM")
	second := memberCall(t, base, linkToken(t, reply, base, "call")).send(t, base, ben)
	if second.Proposal != 2 {
		t.Fatalf("the second proposal is numbered %d", second.Proposal)
	}
	for _, id := range []int64{adaID, benID} {
		reply = chat.ask(id, names[id], "/approve 2")
		memberCall(t, base, linkToken(t, reply, base, "call")).send(t, base, people[id])
	}
	reply = chat.ask(adaID, "ada", "/stop 2")
	mustContain(t, reply, "stop proposal 2")
	mustContain(t, reply, "no timelock")
	third := memberCall(t, base, linkToken(t, reply, base, "call")).send(t, base, ada)
	if third.Proposal != 3 {
		t.Fatalf("the proposal to stop it is numbered %d", third.Proposal)
	}
	for _, id := range []int64{adaID, benID} {
		reply = chat.ask(id, names[id], "/approve 3")
		memberCall(t, base, linkToken(t, reply, base, "call")).send(t, base, people[id])
	}
	// It only stops something, so it waits for nothing.
	mustContain(t, chat.ask(carolID, "carol", "/execute 3"), "Proposal 3 is carried out")
	mustContain(t, chat.ask(benID, "ben", "/execute 2"), "was withdrawn or stopped")

	// ---- The first payment, once its wait is over.

	if wait := time.Until(passedAt.Add(timelock + 8*time.Second)); wait > 0 {
		t.Logf("waiting %s for the timelock", wait.Round(time.Second))
		time.Sleep(wait)
	}
	// Carol carries it out. She has no vote and no key that matters: the
	// decision was made, and anybody may act on it.
	reply = executeWhenReady(t, chat, carolID, "carol", "/execute 1")
	mustContain(t, reply, "Proposal 1 is carried out")
	mustContain(t, reply, "pay 12.00 XLM to "+payee.Address())

	if got, want := native(t, horizon, payee.Address()), plus(t, before, 12); got != want {
		t.Fatalf("the payee holds %s, want %s", got, want)
	}
	mustContain(t, chat.ask(carolID, "carol", "/execute 1"), "has been carried out")
	mustContain(t, chat.ask(carolID, "carol", "/balance"), "28.00 XLM")

	final := chat.ask(carolID, "carol", "/proposals")
	mustContain(t, final, "1 · pay 12.00 XLM")
	mustContain(t, final, "It has been carried out.")
	mustContain(t, final, "2 · pay 3.00 XLM")
	mustContain(t, final, "It was withdrawn or stopped.")

	// ---- The books. Nothing above was a payment the classic network would
	// show: the deposit and the payout were both a token contract moving
	// money, said in events. The server reads those, and its own record of
	// the treasury should come to what the treasury holds — forty in, twelve
	// out — without anybody having told it either number.
	booked := ""
	for attempt := 0; attempt < 45; attempt++ {
		err := db.Pool.QueryRow(context.Background(), `
			SELECT b.balance::text
			  FROM ledger_balances b
			  JOIN ledger_accounts a ON a.id = b.account_id
			  JOIN assets s ON s.id = b.asset_id
			 WHERE a.kind = 'treasury' AND a.name = 'ops' AND s.code = 'XLM'`).Scan(&booked)
		if err == nil && booked == "280000000" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if booked != "280000000" {
		t.Fatalf("the books have the treasury holding %q stroops of XLM, want 280000000 (28 XLM)", booked)
	}
	// Ada's forty came out of a wallet that held it before she linked it,
	// which the books had never seen. That used to stop ingestion where it
	// stood. It is an opening balance now, and said to be one.
	var openings int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_transactions WHERE kind = 'opening'`).Scan(&openings); err != nil {
		t.Fatal(err)
	}
	if openings != 1 {
		t.Fatalf("%d opening balance(s) in the books, want the one for the wallet that deposited", openings)
	}

	// ---- Keeping it alive. The treasury's entries have a lifetime the
	// network charges for, and the server tops them up on a timer. Nothing
	// here will run long enough to see that timer matter, so the operation it
	// sends is sent once by hand: read how long the treasury has left, pay for
	// a little more, and read it again.
	extendOnTestnet(t, payee, treasury)
}

// extendOnTestnet proves the network takes the extension the keeper sends,
// for a real entry, and that the lifetime read back afterwards is longer.
func extendOnTestnet(t *testing.T, payer *keypair.Full, treasury string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, err := settlement.New(settlement.Config{
		HorizonURL: horizonURL, NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}
	client, err = client.DialSoroban(sorobanURL, nil)
	if err != nil {
		t.Fatalf("dial soroban: %v", err)
	}
	instance, err := settlement.ContractInstanceKey(treasury)
	if err != nil {
		t.Fatal(err)
	}
	pinned, _ := contracts.Lookup(contracts.DAOTreasury, contracts.Testnet)
	code, err := settlement.ContractCodeKey(pinned.WasmHash)
	if err != nil {
		t.Fatal(err)
	}

	before, err := client.Lives(ctx, []xdr.LedgerKey{instance, code})
	if err != nil {
		t.Fatalf("read lifetimes: %v", err)
	}
	if !before[0].Found || !before[1].Found {
		t.Fatalf("the treasury (%+v) or its code (%+v) is not on the network", before[0], before[1])
	}
	t.Logf("the treasury has %d ledgers left and its code %d", before[0].Remaining, before[1].Remaining)

	// The treasury's own entry, which is small, by a hundred ledgers: a few
	// minutes more, for a fraction of a cent.
	reach := before[0].Remaining + 100
	prepared, err := client.PrepareExtension(ctx, payer.Address(), []xdr.LedgerKey{instance}, reach, 5_000_000)
	if err != nil {
		t.Fatalf("prepare extension: %v", err)
	}
	signed, err := prepared.Transaction.Sign(network.TestNetworkPassphrase, payer)
	if err != nil {
		t.Fatalf("sign extension: %v", err)
	}
	res, err := client.SubmitSoroban(ctx, signed)
	if err != nil || res.Failed {
		t.Fatalf("the network did not take the extension: %+v, %v", res, err)
	}

	after, err := client.Lives(ctx, []xdr.LedgerKey{instance})
	if err != nil {
		t.Fatalf("read lifetime again: %v", err)
	}
	// Ledgers closed while this ran, so "longer" is judged with that in hand:
	// it has to have gained most of what was paid for.
	if after[0].Remaining+60 < reach {
		t.Fatalf("paid to reach %d ledgers and the treasury has %d", reach, after[0].Remaining)
	}
	t.Logf("extended for %s of rent; it now has %d ledgers left", prepared.ResourceFee, after[0].Remaining)
}

// holdAContractTreasury leaves a treasury held by a contract, with one
// proposal open in it, and two calls waiting for a browser to sign them: a
// vote and a new proposal, each for a different wallet.
//
// Different wallets on purpose. An account has one sequence number, so two
// calls waiting for the same wallet are built for the same one and only the
// first to be signed can land — which the server says when it hands out the
// second link, and which is no state to leave a page-walk in.
func holdAContractTreasury(
	t *testing.T, chat *group, base string, w world, note func(format string, args ...any),
) {
	t.Helper()

	// Ada signs for the classic treasury with a key and no account. A call to
	// a contract has to come from one.
	fund(t, w.horizon, w.ada.Address())
	gail := funded(t, w.horizon, "gail")
	chat.ask(gailID, "gail", "/whoami")
	linkWallet(t, chat, base, gailID, "gail", gail)
	grant(t, chat, gailID, "gail", "approver")

	reply := chat.ask(adaID, "ada", "/create-treasury votes voting:30m timelock:30s recipients:"+w.recipient)
	mustContain(t, reply, "votes is created, and held by a contract at")
	mustContain(t, reply, gail.Address())
	treasury := regexp.MustCompile(`\bC[A-Z2-7]{55}\b`).FindString(reply)
	if treasury == "" {
		t.Fatalf("the reply does not say where the treasury is:\n%s", reply)
	}

	// The first proposal is made here, by Ada, so that there is something for
	// the browser to vote on.
	reply = chat.ask(adaID, "ada", "/pay 1 to "+w.recipient+" asset:XLM treasury:votes")
	if opened := memberCall(t, base, linkToken(t, reply, base, "call")).send(t, base, w.ada); opened.Proposal != 1 {
		t.Fatalf("the held treasury's first proposal is numbered %d", opened.Proposal)
	}

	note("contract treasury: %s", treasury)
	note("gail seed:      %s", gail.Seed())
	note("call vote (gail): %s/call#%s", base,
		linkToken(t, chat.ask(gailID, "gail", "/approve 1 treasury:votes"), base, "call"))
	note("call propose (ada): %s/call#%s", base,
		linkToken(t, chat.ask(adaID, "ada", "/pay 2 to "+w.recipient+" asset:XLM treasury:votes for the browser"), base, "call"))
}

// fund asks friendbot to create an account for an address that has a key and
// nothing else.
func fund(t *testing.T, h *horizonclient.Client, address string) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if _, err = h.Fund(address); err == nil {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("friendbot fund %s: %v", address, err)
}

// linkWallet walks one member through proving a wallet is theirs.
func linkWallet(t *testing.T, chat *group, base string, id int64, name string, wallet *keypair.Full) {
	t.Helper()
	reply := chat.ask(id, name, "/link "+wallet.Address())
	token := linkToken(t, reply, base, "link")
	var challenge struct {
		XDR string `json:"xdr"`
	}
	call(t, "GET", base+"/v1/link", token, nil, http.StatusOK, &challenge)
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, challenge.XDR, wallet)}, http.StatusOK, nil)
}

// grant has the workspace's admin give somebody a role, by the member number
// they read off /whoami.
func grant(t *testing.T, chat *group, id int64, name, role string) {
	t.Helper()
	number := regexp.MustCompile(`member #(\d+)`).FindStringSubmatch(chat.ask(id, name, "/whoami"))
	if number == nil {
		t.Fatalf("/whoami does not tell %s their number", name)
	}
	chat.ask(adaID, "ada", "/grant "+number[1]+" "+role)
}

// pendingCall is what the page a member signs on is given.
type pendingCall struct {
	token     string
	XDR       string `json:"xdr"`
	Hash      string `json:"hash"`
	Canonical string `json:"canonical"`
	Treasury  string `json:"treasury"`
	Member    string `json:"member"`
	Says      string `json:"says"`
	Note      string `json:"note"`
}

type sentCall struct {
	Hash     string `json:"hash"`
	Ledger   int32  `json:"ledger"`
	Function string `json:"function"`
	Proposal uint32 `json:"proposal"`
}

func memberCall(t *testing.T, base, token string) *pendingCall {
	t.Helper()
	c := &pendingCall{token: token}
	call(t, "GET", base+"/v1/call", token, nil, http.StatusOK, c)
	if c.XDR == "" || c.Hash == "" {
		t.Fatalf("the page was given no call: %+v", c)
	}
	return c
}

// send signs the call as its member and submits it, as the page does.
func (c *pendingCall) send(t *testing.T, base string, wallet *keypair.Full) sentCall {
	t.Helper()
	var out sentCall
	call(t, "POST", base+"/v1/call/submit", c.token,
		map[string]string{"signed_xdr": sign(t, c.XDR, wallet)}, http.StatusOK, &out)
	if out.Hash == "" || out.Ledger == 0 {
		t.Fatalf("the call was accepted and nothing is said of where it landed: %+v", out)
	}
	return out
}

// refuse submits the call signed by wallet and expects it to be turned away.
func (c *pendingCall) refuse(t *testing.T, base string, wallet *keypair.Full, want int) {
	t.Helper()
	call(t, "POST", base+"/v1/call/submit", c.token,
		map[string]string{"signed_xdr": sign(t, c.XDR, wallet)}, want, nil)
}

// executeWhenReady asks for a proposal to be carried out, and asks again if
// the network's clock is a few seconds behind this one's. The ledger's clock
// is the one that counts, and the bot says so when that is the reason.
func executeWhenReady(t *testing.T, chat *group, id int64, name, command string) string {
	t.Helper()
	var reply string
	for attempt := 0; attempt < 6; attempt++ {
		reply = chat.ask(id, name, command)
		if !strings.Contains(reply, "timelock") && !strings.Contains(reply, "a few seconds short") {
			return reply
		}
		time.Sleep(6 * time.Second)
	}
	return reply
}

// native is an account's XLM balance.
func native(t *testing.T, h *horizonclient.Client, address string) string {
	t.Helper()
	account, err := h.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		t.Fatalf("load %s: %v", address, err)
	}
	for _, b := range account.Balances {
		if b.Asset.Type == "native" {
			return b.Balance
		}
	}
	t.Fatalf("%s has no XLM balance", address)
	return ""
}
