//go:build e2e

// Run with: make e2e   (go test -tags=e2e ./e2e/ -v)
//
// This is the end-to-end bar from docs/architecture.md, minus the one part a
// test cannot be: the chat platform. Everything else is the real thing.
//
//   - the server is the stelfind binary, built from this tree and run as a
//     separate process, configured through its environment like a deployment;
//   - the database is a real Postgres, migrated by the binary itself at boot;
//   - the network is Stellar testnet, through the public Horizon and friendbot;
//   - Telegram is a local stand-in for the Bot API, reached through the same
//     STELFIN_TELEGRAM_API_URL a self-hosted Bot API server would use. It
//     answers the three methods the transport calls and records what was sent.
//
// The test plays two parts: the people typing in the group, by delivering
// updates to the webhook with the shared secret; and their browsers, by calling
// the same JSON endpoints the signing pages call and signing with their keys.
//
// What it proves is the path a DAO actually walks: set the workspace up, link a
// wallet by signature, prove a 2-of-3 treasury with two signatures, propose a
// payment, be refused execution at one signature, collect the second, execute,
// and find the money at its destination on chain.
//
// Tagged out of the default build for the same reason as the settlement
// integration tests: `go test ./...` must stay runnable offline.
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/settlement"
)

const (
	horizonURL = "https://horizon-testnet.stellar.org"
	sorobanURL = "https://soroban-testnet.stellar.org"
	botToken   = "100200300:e2e-token"
	// Who the stand-in Bot API says the bot is. The transport asks at start,
	// and reads words in a group only when they are said to this name.
	botID         = 100200300
	botUsername   = "stelfin_e2e_bot"
	webhookSecret = "e2e-webhook-secret-0123456789abcdef-0123456789"
	confirmSecret = "e2e-confirm-secret-0123456789abcdef-0123456789"

	groupID = int64(-1009000000001)
	adaID   = int64(4201) // sets the workspace up, proposes, signs first
	benID   = int64(4202) // a second signer, with his own link
	carolID = int64(4203) // has no wallet; writes in her own words
	daveID  = int64(4204) // the same, in a real browser, when E2E_HOLD is set
	eveID   = int64(4205) // brings her own wallet, and signs in it
	gailID  = int64(4206) // votes in a treasury held by a contract, in a real browser

	// Discord. A different platform, with ids from a different space.
	discordApp   = "880000000000000001"
	discordGuild = "770000000000000001"
	frankDiscord = "660000000000000001" // administers the Discord server
	adaDiscord   = "660000000000000002" // ada's account there
)

func TestTreasuryPaymentEndToEnd(t *testing.T) {
	horizon := &horizonclient.Client{HorizonURL: horizonURL, HTTP: &http.Client{Timeout: 60 * time.Second}}

	// ---- The world outside stelfin: accounts, an asset, a funded treasury.

	operator := funded(t, horizon, "operator")
	issuer := funded(t, horizon, "issuer")
	treasury := funded(t, horizon, "treasury")
	recipient := funded(t, horizon, "recipient")
	// The two members hold keys and nothing else. Neither account exists on
	// chain: being a signer on a treasury does not require having one.
	ada, ben := keypair.MustRandom(), keypair.MustRandom()
	webAuth := keypair.MustRandom()

	usdc := txnbuild.CreditAsset{Code: "USDC", Issuer: issuer.Address()}
	line := usdc.MustToChangeTrustAsset()
	one := txnbuild.Threshold(1)
	two := txnbuild.Threshold(2)

	// One transaction: both trustlines, the treasury's float, and its signer
	// set — master, ada and ben at weight one each, needing two to spend.
	submit(t, horizon, treasury, []txnbuild.Operation{
		&txnbuild.ChangeTrust{Line: line, SourceAccount: treasury.Address()},
		&txnbuild.ChangeTrust{Line: line, SourceAccount: recipient.Address()},
		&txnbuild.Payment{
			Destination: treasury.Address(), Amount: "1000", Asset: usdc,
			SourceAccount: issuer.Address(),
		},
		&txnbuild.SetOptions{Signer: &txnbuild.Signer{Address: ada.Address(), Weight: 1}},
		&txnbuild.SetOptions{
			Signer:       &txnbuild.Signer{Address: ben.Address(), Weight: 1},
			MasterWeight: &one, LowThreshold: &two, MediumThreshold: &two, HighThreshold: &two,
		},
	}, treasury, recipient, issuer)
	t.Logf("treasury %s is 2-of-3 and holds 1000 USDC", treasury.Address())

	// ---- stelfin itself.

	db, err := pgtest.Start(freePort(t), func(string) error { return nil })
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Stop() })

	telegram := newFakeTelegram(t)
	discord := newFakeDiscord(t)
	port := freePort(t)
	base := fmt.Sprintf("http://localhost:%d", port)

	// The decoder's model, stood in for. It reads the tokens it is sent and
	// answers with positions in them, which is all the real one is trusted to
	// do: everything it says is re-checked against the user's words.
	model := newFakeModel(t)

	env := map[string]string{
		"STELFIN_HTTP_ADDR":               fmt.Sprintf("127.0.0.1:%d", port),
		"STELFIN_BASE_URL":                base,
		"STELFIN_DATABASE_URL":            db.DSN,
		"STELFIN_NETWORK":                 "testnet",
		"STELFIN_HORIZON_URL":             horizonURL,
		"STELFIN_TREASURY_SEED":           operator.Seed(),
		"STELFIN_WEBAUTH_SEED":            webAuth.Seed(),
		"STELFIN_ASSET_CODE":              "USDC",
		"STELFIN_ASSET_ISSUER":            issuer.Address(),
		"STELFIN_CONFIRM_TOKEN_SECRET":    confirmSecret,
		"STELFIN_TELEGRAM_BOT_TOKEN":      botToken,
		"STELFIN_TELEGRAM_WEBHOOK_SECRET": webhookSecret,
		"STELFIN_TELEGRAM_API_URL":        telegram.URL,
		"STELFIN_SHUTDOWN_GRACE":          "5s",

		"STELFIN_DISCORD_PUBLIC_KEY":     hex.EncodeToString(discord.public),
		"STELFIN_DISCORD_BOT_TOKEN":      "e2e-discord-bot-token",
		"STELFIN_DISCORD_APPLICATION_ID": discordApp,
		"STELFIN_DISCORD_API_URL":        discord.URL,

		// Connectors. The key seals endpoints and tokens; the service account
		// is a throwaway made for this run, which Google will not recognise —
		// so a read gets as far as Google and is refused there, which is as far
		// as a test with no real spreadsheet can honestly go.
		// Contract calls, against testnet's own RPC.
		"STELFIN_SOROBAN_RPC_URL": sorobanURL,
		"STELFIN_CONTRACT_CALLS":  "true",

		"STELFIN_CONNECTOR_KEY":          strings.Repeat("0f", 32),
		"STELFIN_GOOGLE_SERVICE_ACCOUNT": throwawayServiceAccount(t),
	}
	for k, v := range decoderEnv(model) {
		env[k] = v
	}
	startServer(t, base, env)

	// The command menu is published at boot, and Telegram rejects the whole
	// call if any name carries a hyphen.
	for _, name := range telegram.published(t) {
		if strings.Contains(name, "-") {
			t.Errorf("published %q to Telegram, which accepts no hyphens", name)
		}
	}

	chat := &group{t: t, base: base, telegram: telegram}

	// ---- A space nobody set up. Conversation there gets no answer at all;
	// somebody who types a command at the bot is told what has to come first.

	chat.overheard(benID, "ben", "lunch at one?")
	telegram.expectSilence(t, benID, 2*time.Second)
	mustContain(t, chat.ask(benID, "ben", "/whoami"), "isn't set up with stelfin yet")

	// ---- Setup, by an administrator of the group.

	reply := chat.ask(adaID, "ada", "/setup Acme DAO")
	mustContain(t, reply, "Acme DAO")

	// ---- Ada links her wallet: a SEP-10 challenge, signed in "the browser".

	reply = chat.ask(adaID, "ada", "/link "+ada.Address())
	token := linkToken(t, reply, base, "link")
	var challenge struct {
		Address string `json:"address"`
		XDR     string `json:"xdr"`
		Purpose string `json:"purpose"`
	}
	call(t, "GET", base+"/v1/link", token, nil, http.StatusOK, &challenge)
	if challenge.Address != ada.Address() || challenge.Purpose != "link_member" {
		t.Fatalf("challenge is for %s (%s), want ada's wallet", challenge.Address, challenge.Purpose)
	}
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, challenge.XDR, ada)}, http.StatusOK, nil)

	// The challenge is spent. Presenting it again must not work.
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, challenge.XDR, ada)}, http.StatusNotFound, nil)

	mustContain(t, chat.ask(adaID, "ada", "/whoami"), ada.Address())

	// ---- The treasury, proved with the weight a payment needs.

	// Typed the way the bot's own replies spell it, hyphen and all.
	reply = chat.ask(adaID, "ada", "/link-treasury "+treasury.Address())
	token = linkToken(t, reply, base, "link")
	call(t, "GET", base+"/v1/link", token, nil, http.StatusOK, &challenge)
	if challenge.Purpose != "link_treasury" {
		t.Fatalf("purpose = %q, want link_treasury", challenge.Purpose)
	}

	// One signature of the two it needs proves nothing.
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, challenge.XDR, ada), "label": "ops"},
		http.StatusBadRequest, nil)

	var proved struct {
		Address   string   `json:"address"`
		SignedBy  []string `json:"signed_by"`
		Threshold int      `json:"threshold"`
	}
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, challenge.XDR, ada, ben), "label": "ops"},
		http.StatusOK, &proved)
	if proved.Address != treasury.Address() || proved.Threshold != 2 || len(proved.SignedBy) != 2 {
		t.Fatalf("treasury proof = %+v", proved)
	}

	mustContain(t, chat.ask(adaID, "ada", "/treasuries"), treasury.Address())
	// Typed the way Telegram's menu offers it, with an underscore.
	mustContain(t, chat.ask(adaID, "ada", "/link_code"), "/claim ")

	// Discord takes hyphens, so there the commands keep the names they were
	// declared with — and the admin-only ones are hidden from everyone else.
	if names := discord.published(t); !contains(names, "link-treasury") || !contains(names, "pay") {
		t.Errorf("the command menu published to Discord is %v", names)
	}

	// ---- A payment, proposed in chat.

	reply = chat.ask(adaID, "ada", "/pay 25 to "+recipient.Address()+" march invoice")
	mustContain(t, reply, "Proposal #1")
	adaToken := linkToken(t, reply, base, "approve")

	var proposal struct {
		XDR       string   `json:"xdr"`
		Canonical string   `json:"canonical"`
		Have      int      `json:"have"`
		Need      int      `json:"need"`
		Ready     bool     `json:"ready"`
		Missing   []string `json:"missing"`
	}
	call(t, "GET", base+"/v1/proposal", adaToken, nil, http.StatusOK, &proposal)
	if proposal.Need != 2 || proposal.Have != 0 {
		t.Fatalf("fresh proposal has %d of %d", proposal.Have, proposal.Need)
	}
	// What the signers are shown is derived from the envelope, so it has to
	// name this destination and this amount.
	mustContain(t, proposal.Canonical, recipient.Address())
	mustContain(t, proposal.Canonical, "25")
	t.Logf("what the approver is shown:\n%s", proposal.Canonical)

	// A stranger's signature is a valid signature and worth nothing.
	call(t, "POST", base+"/v1/proposal/approve", adaToken,
		map[string]string{"signed_xdr": sign(t, proposal.XDR, keypair.MustRandom())},
		http.StatusBadRequest, nil)

	call(t, "POST", base+"/v1/proposal/approve", adaToken,
		map[string]string{"signed_xdr": sign(t, proposal.XDR, ada)}, http.StatusOK, &proposal)
	if proposal.Have != 1 || proposal.Ready {
		t.Fatalf("after one signature: have %d, ready %v", proposal.Have, proposal.Ready)
	}

	// One of two. The network would refuse it; stelfin must not even try.
	mustContain(t, chat.ask(adaID, "ada", "/execute 1"), "weight 1 of 2")
	if got := balance(t, horizon, recipient.Address(), usdc); got != "0.0000000" {
		t.Fatalf("recipient holds %s before the proposal has its signatures", got)
	}

	// ---- The second signer, on his own link.

	// Ben has never spoken here and holds no role, so the bot offers him
	// nothing — not even a link his signature would be good for.
	mustContain(t, chat.ask(benID, "ben", "/approve 1"), "needs the observer role")

	// He reads his member number off /whoami and an admin grants him a role.
	number := regexp.MustCompile(`member #(\d+)`).FindStringSubmatch(chat.ask(benID, "ben", "/whoami"))
	if number == nil {
		t.Fatal("/whoami does not tell a member their number")
	}
	mustContain(t, chat.ask(benID, "ben", "/grant "+number[1]+" admin"), "needs the admin role")
	mustContain(t, chat.ask(adaID, "ada", "/grant "+number[1]+" approver"), "an approver")

	reply = chat.ask(benID, "ben", "/approve 1")
	benToken := linkToken(t, reply, base, "approve")
	call(t, "GET", base+"/v1/proposal", benToken, nil, http.StatusOK, &proposal)
	// Ben signs the bare envelope. His signature is stored beside ada's, not on
	// top of it — the envelope is rebuilt from both.
	call(t, "POST", base+"/v1/proposal/approve", benToken,
		map[string]string{"signed_xdr": sign(t, proposal.XDR, ben)}, http.StatusOK, &proposal)
	if proposal.Have != 2 || !proposal.Ready {
		t.Fatalf("after two signatures: have %d, ready %v", proposal.Have, proposal.Ready)
	}

	// ---- Execution, and the only check that finally matters.

	reply = chat.ask(benID, "ben", "/execute")
	mustContain(t, reply, "on chain")
	t.Log(strings.ReplaceAll(reply, "\n", " "))

	if got := balance(t, horizon, recipient.Address(), usdc); got != "25.0000000" {
		t.Fatalf("recipient holds %s USDC, want 25", got)
	}
	if got := balance(t, horizon, treasury.Address(), usdc); got != "975.0000000" {
		t.Fatalf("treasury holds %s USDC, want 975", got)
	}

	// Executing twice must not pay twice.
	mustContain(t, chat.ask(adaID, "ada", "/execute 1"), "already closed")
	mustContain(t, chat.ask(adaID, "ada", "/proposals"), "#1")
	if got := balance(t, horizon, recipient.Address(), usdc); got != "25.0000000" {
		t.Fatalf("recipient holds %s USDC after a second /execute, want 25", got)
	}

	// The treasury is free again: a second proposal opens.
	mustContain(t, chat.ask(adaID, "ada", "/pay amount:5 to:"+recipient.Address()), "Proposal #2")

	// ---- In her own words: the wallet stelfin sets up, behind its limiter.

	plain := "send 3 to " + recipient.Address()

	// Off until the workspace turns it on, and said in chat rather than left
	// for a page to discover.
	mustContain(t, chat.ask(carolID, "carol", plain), "/provisioning on")
	mustContain(t, chat.ask(carolID, "carol", "/provisioning on"), "needs the admin role")
	mustContain(t, chat.ask(adaID, "ada", "/provisioning on 5"), "5 wallets a day")

	// Carol was first seen a minute ago, in a workspace made a few minutes ago.
	// The operator does not pay for accounts on that acquaintance — and she is
	// told so in chat, and when to come back, rather than handed a link to a
	// page that could only refuse her.
	reply = chat.ask(carolID, "carol", plain)
	mustContain(t, reply, "not quite yet")
	if strings.Contains(reply, "/enroll#") {
		t.Fatalf("somebody the limiter will refuse was handed a link:\n%s", reply)
	}

	// Only time makes something old, so the test supplies the time.
	age(t, db)
	token = linkToken(t, chat.ask(carolID, "carol", plain), base, "enroll")
	carol := keypair.MustRandom() // generated on her device; never leaves it
	var provision struct {
		Address string `json:"address"`
		XDR     string `json:"xdr"`
	}
	call(t, "POST", base+"/v1/enroll", token, map[string]string{"address": carol.Address()},
		http.StatusOK, &provision)
	signedProvision := sign(t, provision.XDR, carol)

	// What she was told in chat was a courtesy. The limiter is what decides,
	// in the step that commits the operator to paying, and it asks again: the
	// test takes the time back, and the same signed request is refused with
	// nothing created.
	youth(t, db)
	call(t, "POST", base+"/v1/enroll/submit", token,
		map[string]string{"signed_xdr": signedProvision}, http.StatusTooEarly, nil)
	if _, err := horizon.AccountDetail(horizonclient.AccountRequest{AccountID: carol.Address()}); !horizonclient.IsNotFoundError(err) {
		t.Fatalf("a refused enrolment created the account anyway (lookup: %v)", err)
	}

	age(t, db)
	call(t, "POST", base+"/v1/enroll/submit", token,
		map[string]string{"signed_xdr": signedProvision}, http.StatusOK, nil)

	// A real account, holding no XLM at all: the operator carries every reserve.
	account, err := horizon.AccountDetail(horizonclient.AccountRequest{AccountID: carol.Address()})
	if err != nil {
		t.Fatalf("carol's account: %v", err)
	}
	if native, _ := account.GetNativeBalance(); native != "0.0000000" {
		t.Errorf("carol's new account holds %s XLM, want none", native)
	}
	if account.Sponsor != operator.Address() || account.NumSponsored == 0 {
		t.Errorf("carol's account is sponsored by %q (%d entries), want the operator",
			account.Sponsor, account.NumSponsored)
	}

	// She has one now, so she is not offered another.
	call(t, "POST", base+"/v1/enroll", token,
		map[string]string{"address": keypair.MustRandom().Address()}, http.StatusConflict, nil)

	// Somebody pays her, and she pays somebody — typed as a sentence.
	submit(t, horizon, issuer, []txnbuild.Operation{
		&txnbuild.Payment{Destination: carol.Address(), Amount: "10", Asset: usdc},
	}, issuer)
	before := balance(t, horizon, recipient.Address(), usdc)

	// The model is somebody else's API, and it goes down. A sentence written
	// plainly is read anyway, by a grammar that needs no model; one that is not
	// is answered with how to write it so that it goes through, and is not
	// called a mistake.
	model.down.Store(true)
	mustContain(t, chat.ask(carolID, "carol", plain), "Send 3")
	mustContain(t, chat.ask(carolID, "carol", "could you send three to "+recipient.Address()), "isn't answering")
	model.down.Store(false)

	reply = chat.ask(carolID, "carol", plain)
	mustContain(t, reply, "Send 3")
	token = linkToken(t, reply, base, "confirm")
	var confirmation struct {
		XDR       string `json:"xdr"`
		Amount    string `json:"amount"`
		ToAddress string `json:"to_address"`
		From      string `json:"from_address"`
		Said      string `json:"said_amount"`
	}
	call(t, "GET", base+"/v1/confirm", token, nil, http.StatusOK, &confirmation)
	if confirmation.ToAddress != recipient.Address() || confirmation.From != carol.Address() || confirmation.Said != "3" {
		t.Fatalf("confirmation = %+v", confirmation)
	}
	call(t, "POST", base+"/v1/submit", token,
		map[string]string{"signed_xdr": sign(t, confirmation.XDR, carol)}, http.StatusOK, nil)
	// A confirmation is good once.
	call(t, "POST", base+"/v1/submit", token,
		map[string]string{"signed_xdr": sign(t, confirmation.XDR, carol)}, http.StatusConflict, nil)

	if got := balance(t, horizon, carol.Address(), usdc); got != "7.0000000" {
		t.Fatalf("carol holds %s USDC after sending 3 of 10", got)
	}
	if got, want := balance(t, horizon, recipient.Address(), usdc), plus(t, before, 3); got != want {
		t.Fatalf("recipient holds %s USDC, want %s", got, want)
	}
	// She paid no fee, because she has no XLM to pay one with.
	account, _ = horizon.AccountDetail(horizonclient.AccountRequest{AccountID: carol.Address()})
	if native, _ := account.GetNativeBalance(); native != "0.0000000" {
		t.Errorf("carol holds %s XLM after a payment; the operator should have paid the fee", native)
	}

	mustContain(t, chat.ask(carolID, "carol", "/whoami"), carol.Address())

	// ---- Handing it back. Not while there is money in it.

	mustContain(t, chat.ask(carolID, "carol", "/reclaim"), "Not yet")

	token = linkToken(t, chat.ask(carolID, "carol", "send 7 to "+recipient.Address()), base, "confirm")
	call(t, "GET", base+"/v1/confirm", token, nil, http.StatusOK, &confirmation)
	call(t, "POST", base+"/v1/submit", token,
		map[string]string{"signed_xdr": sign(t, confirmation.XDR, carol)}, http.StatusOK, nil)

	token = linkToken(t, chat.ask(carolID, "carol", "/reclaim"), base, "reclaim")
	var handBack struct {
		XDR         string `json:"xdr"`
		Destination string `json:"destination"`
	}
	call(t, "GET", base+"/v1/reclaim", token, nil, http.StatusOK, &handBack)
	if handBack.Destination != operator.Address() {
		t.Fatalf("the account would be swept to %s, want the operator", handBack.Destination)
	}
	call(t, "POST", base+"/v1/reclaim/submit", token,
		map[string]string{"signed_xdr": sign(t, handBack.XDR, carol)}, http.StatusOK, nil)
	if _, err := horizon.AccountDetail(horizonclient.AccountRequest{AccountID: carol.Address()}); !horizonclient.IsNotFoundError(err) {
		t.Fatalf("carol's account still exists after being handed back (lookup: %v)", err)
	}

	// ---- Limits the workspace sets itself, and a payment that could not land.

	mustContain(t, chat.ask(adaID, "ada", "/limits payment 100 daily 1000"), "one proposal: 100")
	mustContain(t, chat.ask(adaID, "ada", "/pay 500 to "+recipient.Address()), "/limits")
	// An account that does not exist. Found now, before anyone signs.
	mustContain(t, chat.ask(adaID, "ada", "/pay 1 to "+keypair.MustRandom().Address()), "wouldn't land")

	// ---- A connector: registered by an admin, granted by the signers.

	mustContain(t, chat.ask(adaID, "ada", "/connector add payroll sheets"), "grants it nothing")
	// Registered is not granted, and an ungranted connector is not called.
	mustContain(t, chat.ask(adaID, "ada", "/read payroll SHEET!A1:B2"), "hasn't been granted")

	reply = chat.ask(adaID, "ada", "/connector grant payroll 20")
	mustContain(t, reply, "20 calls an hour")
	token = linkToken(t, reply, base, "link")
	var grant struct {
		XDR       string `json:"xdr"`
		Purpose   string `json:"purpose"`
		Statement string `json:"statement"`
	}
	call(t, "GET", base+"/v1/link", token, nil, http.StatusOK, &grant)
	if grant.Purpose != "grant_connector" {
		t.Fatalf("purpose = %q, want grant_connector", grant.Purpose)
	}
	// What a signer's browser checks: the envelope commits to the hash of
	// exactly the text that was shown, and can never be submitted.
	if committed := statementIn(t, grant.XDR); committed != sha256Hex(grant.Statement) {
		t.Fatalf("the envelope commits to %s, and the statement shown hashes to %s",
			committed, sha256Hex(grant.Statement))
	}
	for _, want := range []string{"connector\tpayroll", "may\tread.range", "treasury\t" + treasury.Address()} {
		mustContain(t, grant.Statement, want)
	}

	// An admin alone cannot put it in force, and nor can one signer of two.
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, grant.XDR, ada)}, http.StatusBadRequest, nil)
	mustContain(t, chat.ask(adaID, "ada", "/read payroll SHEET!A1:B2"), "hasn't been granted")

	var granted struct {
		Connector string   `json:"connector"`
		SignedBy  []string `json:"signed_by"`
	}
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, grant.XDR, ada, ben)}, http.StatusOK, &granted)
	if granted.Connector != "payroll" || len(granted.SignedBy) != 2 {
		t.Fatalf("grant = %+v", granted)
	}
	mustContain(t, chat.ask(adaID, "ada", "/connectors"), "granted, 20 calls an hour, signed by 2")

	// Granted, so now it is called — and Google does not know this run's
	// throwaway key. The member is told it could not be read, and nothing of
	// what Google said about why.
	reply = chat.ask(adaID, "ada", "/read payroll SHEET!A1:B2")
	mustContain(t, reply, "couldn't read")
	if strings.Contains(reply, "invalid_grant") || strings.Contains(reply, "oauth2") {
		t.Errorf("the reply carries the upstream's own words:\n%s", reply)
	}

	// Stopping one takes nobody's signature.
	mustContain(t, chat.ask(adaID, "ada", "/connector revoke payroll"), "Revoked")
	mustContain(t, chat.ask(adaID, "ada", "/read payroll SHEET!A1:B2"), "hasn't been granted")

	// A connector with an address gives it on a page — and the page will not
	// take one that leads somewhere private. These are refused before anything
	// is dialled: this machine, by name and by number, and plain http.
	reply = chat.ask(adaID, "ada", "/connector add rates webhook")
	token = linkToken(t, reply, base, "link")
	var setup struct {
		Purpose   string `json:"purpose"`
		Connector struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"connector"`
	}
	call(t, "GET", base+"/v1/link", token, nil, http.StatusOK, &setup)
	if setup.Purpose != "connector_setup" || setup.Connector.Name != "rates" || setup.Connector.Ready {
		t.Fatalf("set-up view = %+v", setup)
	}
	for _, private := range []string{
		telegram.URL, // http, and loopback
		"https://127.0.0.1/rows",
		"https://localhost/rows",
		"https://169.254.169.254/latest/meta-data/",
		"https://user:pass@example.com/rows",
	} {
		call(t, "POST", base+"/v1/connector/setup", token,
			map[string]string{"endpoint": private}, http.StatusBadRequest, nil)
	}
	// A real public address that does not serve rows is reached, through the
	// pinned dialer, and reported as not useful. Nothing is saved.
	call(t, "POST", base+"/v1/connector/setup", token,
		map[string]string{"endpoint": horizonURL + "/"}, http.StatusUnprocessableEntity, nil)
	mustContain(t, chat.ask(adaID, "ada", "/connector grant rates"), "hasn't been set up")

	// ---- A member who brings her own wallet, and signs in it.

	eve := funded(t, horizon, "eve")
	submit(t, horizon, eve, []txnbuild.Operation{
		&txnbuild.ChangeTrust{Line: line},
		&txnbuild.Payment{Destination: eve.Address(), Amount: "10", Asset: usdc, SourceAccount: issuer.Address()},
	}, eve, issuer)
	token = linkToken(t, chat.ask(eveID, "eve", "/link "+eve.Address()), base, "link")
	call(t, "GET", base+"/v1/link", token, nil, http.StatusOK, &challenge)
	call(t, "POST", base+"/v1/link/submit", token,
		map[string]string{"signed_xdr": sign(t, challenge.XDR, eve)}, http.StatusOK, nil)

	before = balance(t, horizon, recipient.Address(), usdc)
	token = linkToken(t, chat.ask(eveID, "eve", "send 2 to "+recipient.Address()), base, "confirm")
	call(t, "GET", base+"/v1/confirm", token, nil, http.StatusOK, &confirmation)
	if confirmation.From != eve.Address() {
		t.Fatalf("eve's payment would come from %s, want the wallet she linked", confirmation.From)
	}
	call(t, "POST", base+"/v1/submit", token,
		map[string]string{"signed_xdr": sign(t, confirmation.XDR, eve)}, http.StatusOK, nil)
	if got, want := balance(t, horizon, recipient.Address(), usdc), plus(t, before, 2); got != want {
		t.Fatalf("recipient holds %s USDC after eve's payment, want %s", got, want)
	}

	// ---- An outside agent, reading over MCP with a token minted in chat.

	reply = chat.ask(adaID, "ada", "/agent-token reporting")
	agent := regexp.MustCompile(`(?m)^(\S{40,})$`).FindString(reply)
	if agent == "" {
		t.Fatalf("no token in:\n%s", reply)
	}
	ask := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stelfin_treasuries","arguments":{}}}`
	if code, body := post(t, base+"/mcp", agent, ask); code != http.StatusOK || !strings.Contains(body, treasury.Address()) {
		t.Fatalf("the agent's read answered %d: %s", code, body)
	}
	if code, _ := post(t, base+"/mcp", "", ask); code != http.StatusUnauthorized {
		t.Errorf("a read with no token answered %d, want 401", code)
	}
	mustContain(t, chat.ask(adaID, "ada", "/agent-revoke 1"), "revoked")
	if code, _ := post(t, base+"/mcp", agent, ask); code != http.StatusUnauthorized {
		t.Errorf("a revoked token answered %d, want 401", code)
	}

	// ---- The same workspace, from Discord.

	guild := &server{t: t, base: base, discord: discord}

	// A server the bot was added to and nobody connected. Discord has already
	// been told an answer is coming, so silence would be a spinner that never
	// stops; the answer says nothing about the workspace it has not joined.
	mustContain(t, guild.ask(frankDiscord, "frank", true, "proposals", nil), "isn't set up with stelfin yet")

	// A code from an admin of the workspace, presented by an administrator of
	// the server: each side proving the half it can.
	found := regexp.MustCompile(`/connect ([A-Z2-9]{16})`).FindStringSubmatch(chat.ask(adaID, "ada", "/connect"))
	if found == nil {
		t.Fatal("/connect gave no code")
	}
	mustContain(t, guild.ask(adaDiscord, "ada", false, "connect", map[string]string{"code": found[1]}), "Only an administrator")
	mustContain(t, guild.ask(frankDiscord, "frank", true, "connect", map[string]string{"code": found[1]}), "Acme DAO")
	// Connecting the room did not hand anyone in it a role.
	mustContain(t, guild.ask(frankDiscord, "frank", true, "proposals", nil), "needs the observer role")

	// Ada is one person with two chat accounts. A code from the account that
	// has proved a wallet attaches the other, and she is the same member —
	// role, wallet and all — on both.
	found = regexp.MustCompile(`/claim (\S+)`).FindStringSubmatch(chat.ask(adaID, "ada", "/link_code"))
	if found == nil {
		t.Fatal("/link_code gave no code")
	}
	mustContain(t, guild.ask(adaDiscord, "ada", false, "claim", map[string]string{"code": found[1]}), ada.Address())
	who := guild.ask(adaDiscord, "ada", false, "whoami", nil)
	mustContain(t, who, "Role: admin")
	mustContain(t, who, ada.Address())

	// Granting from Discord's own member picker, which hands over an id.
	mustContain(t, guild.ask(adaDiscord, "ada", false, "grant",
		map[string]string{"role": "observer", "user": frankDiscord}), "an observer")
	mustContain(t, guild.ask(frankDiscord, "frank", true, "proposals", nil), "#2")

	// Signing the proposal opened on Telegram, from a link handed out on Discord.
	token = linkToken(t, guild.ask(adaDiscord, "ada", false, "approve", map[string]string{"id": "2"}), base, "approve")
	call(t, "GET", base+"/v1/proposal", token, nil, http.StatusOK, &proposal)
	call(t, "POST", base+"/v1/proposal/approve", token,
		map[string]string{"signed_xdr": sign(t, proposal.XDR, ada)}, http.StatusOK, &proposal)
	if proposal.Have != 1 {
		t.Fatalf("after a signature collected through Discord: have %d", proposal.Have)
	}

	// A sentence, which on Discord has to arrive as a command. Ada's linked
	// wallet is a signer key that was never funded, and she is told so.
	mustContain(t, guild.ask(adaDiscord, "ada", false, "send",
		map[string]string{"what": "1 to " + recipient.Address()}), "doesn't exist on this network")

	// ---- A third-party contract, called from the treasury.

	// Finish proposal #2 first: one account, one open proposal. Ada signed it
	// from Discord; ben adds his signature and anyone may submit.
	token = linkToken(t, chat.ask(benID, "ben", "/approve 2"), base, "approve")
	call(t, "GET", base+"/v1/proposal", token, nil, http.StatusOK, &proposal)
	call(t, "POST", base+"/v1/proposal/approve", token,
		map[string]string{"signed_xdr": sign(t, proposal.XDR, ben)}, http.StatusOK, &proposal)
	mustContain(t, chat.ask(benID, "ben", "/execute 2"), "on chain")

	// The asset's own contract, deployed for this run's USDC. It is the one
	// contract every classic asset can have, and a real one to call.
	usdcContract := deployAssetContract(t, issuer, usdc)
	t.Logf("asset contract %s", usdcContract)

	mustContain(t, chat.ask(adaID, "ada", "/contract allow "+usdcContract+" usdc"), "authorises no call")
	mustContain(t, chat.ask(adaID, "ada", "/contracts"), "usdc — "+usdcContract)
	// A typed argument, or nothing.
	mustContain(t, chat.ask(adaID, "ada", "/invoke usdc transfer "+treasury.Address()+" 5"), "type:value")

	before = balance(t, horizon, recipient.Address(), usdc)
	reply = chat.ask(adaID, "ada", "/invoke usdc transfer address:"+treasury.Address()+
		" address:"+recipient.Address()+" i128:50000000")
	mustContain(t, reply, "limits on payments here don't apply")
	token = linkToken(t, reply, base, "approve")
	call(t, "GET", base+"/v1/proposal", token, nil, http.StatusOK, &proposal)
	// What the signers see is the call, read from its own envelope.
	for _, want := range []string{"invoke", usdcContract, "transfer", recipient.Address()} {
		mustContain(t, proposal.Canonical, want)
	}
	call(t, "POST", base+"/v1/proposal/approve", token,
		map[string]string{"signed_xdr": sign(t, proposal.XDR, ada)}, http.StatusOK, &proposal)
	mustContain(t, chat.ask(adaID, "ada", "/execute"), "weight 1 of 2")
	call(t, "POST", base+"/v1/proposal/approve", token,
		map[string]string{"signed_xdr": sign(t, proposal.XDR, ben)}, http.StatusOK, &proposal)
	reply = chat.ask(benID, "ben", "/execute")
	mustContain(t, reply, "on chain")
	t.Log(strings.ReplaceAll(reply, "\n", " "))
	if got, want := balance(t, horizon, recipient.Address(), usdc), plus(t, before, 5); got != want {
		t.Fatalf("after the contract call the recipient holds %s USDC, want %s", got, want)
	}

	// ---- What the treasury holds, and what it can be asked to hold or
	// exchange. Each is a proposal like a payment: built here, signed by the
	// treasury's own signers, and settled on the network.

	reply = chat.ask(adaID, "ada", "/balance")
	mustContain(t, reply, "USDC")
	mustContain(t, reply, treasury.Address())
	mustContain(t, reply, "Read from the network just now")

	// A second asset, issued for this run. The treasury cannot be given it
	// until it has a trustline, and is told so before anybody signs anything.
	eurc := txnbuild.CreditAsset{Code: "EURC", Issuer: issuer.Address()}
	mustContain(t, chat.ask(adaID, "ada", "/swap 10 USDC to EURC:"+issuer.Address()), "/trust")
	// A bare code names nothing yet: anybody can issue an asset called EURC.
	mustContain(t, chat.ask(adaID, "ada", "/trust EURC"), "name its issuer")

	approveAndExecute := func(reply string) {
		t.Helper()
		token := linkToken(t, reply, base, "approve")
		call(t, "GET", base+"/v1/proposal", token, nil, http.StatusOK, &proposal)
		call(t, "POST", base+"/v1/proposal/approve", token,
			map[string]string{"signed_xdr": sign(t, proposal.XDR, ada, ben)}, http.StatusOK, &proposal)
		mustContain(t, chat.ask(benID, "ben", "/execute"), "on chain")
	}

	reply = chat.ask(adaID, "ada", "/trust EURC:"+issuer.Address())
	mustContain(t, reply, "0.5 XLM")
	approveAndExecute(reply)
	if got := balance(t, horizon, treasury.Address(), eurc); got != "0.0000000" {
		t.Fatalf("after /trust the treasury holds %q EURC, want an empty trustline", got)
	}
	// Twice is not a second proposal.
	mustContain(t, chat.ask(adaID, "ada", "/trust EURC:"+issuer.Address()), "already has a trustline")

	// Nobody is offering EURC for USDC, and that is the answer: no price is
	// made up, and nothing is proposed.
	mustContain(t, chat.ask(adaID, "ada", "/swap 10 USDC to EURC"), "Nobody on this network is offering")

	// Somebody places an order: 100 EURC for sale at 2 USDC each.
	submit(t, horizon, issuer, []txnbuild.Operation{&txnbuild.ManageSellOffer{
		Selling: eurc, Buying: usdc, Amount: "100", Price: xdr.Price{N: 2, D: 1},
	}}, issuer)

	// Asked with no floor, the market's answer and no proposal.
	//
	// Asked again if the market has not heard of the order yet. Horizon
	// answers a quote from an order book it builds a ledger or so behind the
	// ledger itself, so for a few seconds after an order lands the true answer
	// is still "nobody is offering that" — and the bot gives it.
	usdcBefore := balance(t, horizon, treasury.Address(), usdc)
	for attempt := 0; attempt < 10; attempt++ {
		reply = chat.ask(adaID, "ada", "/swap 10 USDC to EURC")
		if !strings.Contains(reply, "Nobody on this network is offering") {
			break
		}
		time.Sleep(3 * time.Second)
	}
	mustContain(t, reply, "about 5.00 EURC")
	mustContain(t, reply, "Nothing has been proposed")
	// A floor above what the market offers would fail on the ledger, and is
	// refused before it can.
	mustContain(t, chat.ask(adaID, "ada", "/swap 10 USDC to EURC min:6"), "will not take less than 6.00")

	reply = chat.ask(adaID, "ada", "/swap 10 USDC to EURC slippage:1%")
	mustContain(t, reply, "at least 4.95 EURC")
	approveAndExecute(reply)
	if got := balance(t, horizon, treasury.Address(), eurc); got != "5.0000000" {
		t.Fatalf("after the swap the treasury holds %s EURC, want 5", got)
	}
	if got, want := balance(t, horizon, treasury.Address(), usdc), plus(t, usdcBefore, -10); got != want {
		t.Fatalf("after the swap the treasury holds %s USDC, want %s", got, want)
	}
	mustContain(t, chat.ask(adaID, "ada", "/balance"), "5.00 EURC:"+issuer.Address())

	// A trustline is closed only empty.
	mustContain(t, chat.ask(adaID, "ada", "/untrust EURC"), "still holds 5.00")

	// ---- A proposal can be taken back. One account holds one open proposal,
	// so a mistyped /pay would otherwise block every later one until its
	// window ran out.

	reply = chat.ask(adaID, "ada", "/pay 9 to "+recipient.Address())
	mistaken := regexp.MustCompile(`Proposal #(\d+)`).FindStringSubmatch(reply)
	if mistaken == nil {
		t.Fatalf("/pay opened nothing:\n%s", reply)
	}
	// The refusal of a second one names the way out.
	mustContain(t, chat.ask(adaID, "ada", "/pay 5 to "+recipient.Address()), "/cancel "+mistaken[1])
	// Ben holds a role, and it is not his to withdraw.
	mustContain(t, chat.ask(benID, "ben", "/cancel"), "an admin")
	mustContain(t, chat.ask(adaID, "ada", "/cancel"), "withdrawn")
	mustContain(t, chat.ask(adaID, "ada", "/proposals"), "cancelled")

	// One more payment left open, for the browser to sign if this run is held.
	mustContain(t, chat.ask(adaID, "ada", "/pay 5 to "+recipient.Address()), "Proposal #")

	holdForABrowser(t, chat, base, world{
		db: db, horizon: horizon, issuer: issuer, usdc: usdc,
		treasury: treasury.Address(), recipient: recipient.Address(), ada: ada, ben: ben, eve: eve,
	})

	// ---- A delivery without the secret is refused, and so is a replay.

	if code := chat.deliver(adaID, "ada", "/whoami", "wrong-secret", 0); code != http.StatusForbidden {
		t.Errorf("a delivery with the wrong secret got %d, want 403", code)
	}
	replayed := chat.nextUpdate()
	chat.deliver(adaID, "ada", "/whoami", webhookSecret, replayed)
	telegram.next(t, adaID)
	chat.deliver(adaID, "ada", "/whoami", webhookSecret, replayed)
	telegram.expectSilence(t, adaID, 2*time.Second)
}

// holdForABrowser keeps the server up with proposal #2 open, so a person can
// walk the signing pages themselves. Off unless E2E_HOLD names a duration:
//
//	E2E_HOLD=10m make e2e
//
// It writes fresh links and the two signers' throwaway testnet seeds to
// E2E_HOLD_FILE (default e2e-hold.txt). The pages sign with a key pasted into
// them or with a wallet; these keys exist only for this run and hold nothing.
func holdForABrowser(t *testing.T, chat *group, base string, w world) {
	t.Helper()
	raw := os.Getenv("E2E_HOLD")
	if raw == "" {
		return
	}
	wait, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("E2E_HOLD: %v", err)
	}
	deadline := time.Now().Add(wait)
	path := os.Getenv("E2E_HOLD_FILE")
	if path == "" {
		path = "e2e-hold.txt"
	}
	note := func(format string, args ...any) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer f.Close()
		fmt.Fprintf(f, format+"\n", args...)
	}
	_ = os.Remove(path)
	_ = os.Remove(path + ".done")

	fresh := keypair.MustRandom()
	linkReply := chat.ask(benID, "ben", "/link "+fresh.Address())
	treasuryReply := chat.ask(adaID, "ada", "/link-treasury "+w.treasury)
	note("approve (ada):  %s/approve#%s", base, linkToken(t, chat.ask(adaID, "ada", "/approve"), base, "approve"))
	note("approve (ben):  %s/approve#%s", base, linkToken(t, chat.ask(benID, "ben", "/approve"), base, "approve"))
	note("link (ben):     %s/link#%s", base, linkToken(t, linkReply, base, "link"))
	note("treasury (ada): %s/link#%s", base, linkToken(t, treasuryReply, base, "link"))
	note("ada seed:       %s", w.ada.Seed())
	note("ben seed:       %s", w.ben.Seed())
	note("ben new wallet: %s", fresh.Seed())
	// The two connector pages: a grant for the signers to read and sign, and a
	// connector waiting to be told where it is.
	note("grant (ada):    %s/link#%s", base,
		linkToken(t, chat.ask(adaID, "ada", "/connector grant payroll 20"), base, "link"))
	note("setup (ada):    %s/link#%s", base,
		linkToken(t, chat.ask(adaID, "ada", "/connector setup rates"), base, "link"))
	// Eve's wallet is her own, so her page has no key to sign with and offers
	// the transaction to be signed elsewhere.
	note("eve seed:       %s", w.eve.Seed())
	note("confirm (eve):  %s/confirm#%s", base,
		linkToken(t, chat.ask(eveID, "eve", "send 1 to "+w.recipient), base, "confirm"))

	// A treasury held by a contract, for the page a member signs a call on.
	// Made last of the things above, because once a workspace has two
	// treasuries a command about "the open proposal" is asked which it means —
	// and before Dave's link below, which is good for two minutes.
	holdAContractTreasury(t, chat, base, w, note)

	// Dave has no wallet. His three pages come one at a time, because each
	// depends on what he did on the one before: the account his browser made,
	// then the money somebody sent it, then its being empty again.
	plain := "send 3 to " + w.recipient
	chat.ask(daveID, "dave", "/whoami")
	age(t, w.db)
	note("enroll (dave):  %s/enroll#%s", base, linkToken(t, chat.ask(daveID, "dave", plain), base, "enroll"))
	t.Logf("holding for %s; links and throwaway keys are in %s", wait, path)

	// Whoever is walking the pages can end the hold early by creating
	// <hold-file>.done, which is how `make e2e-browser` avoids sitting out the
	// rest of the duration once it has seen everything.
	released := func() bool {
		_, err := os.Stat(path + ".done")
		return err == nil
	}
	until := func(what string, done func() bool) bool {
		for time.Now().Before(deadline) {
			if released() {
				return false
			}
			if done() {
				return true
			}
			time.Sleep(2 * time.Second)
		}
		if !released() {
			t.Logf("hold ended before %s", what)
		}
		return false
	}

	var wallet string
	if !until("dave set his wallet up", func() bool {
		err := w.db.Pool.QueryRow(context.Background(), `
			SELECT t.address FROM tracked_addresses t
			  JOIN ledger_accounts a ON a.id = t.ledger_account_id
			 WHERE a.kind = 'member' AND a.owner_ref = $1`,
			fmt.Sprintf("telegram:%d", daveID)).Scan(&wallet)
		return err == nil
	}) {
		return
	}
	submit(t, w.horizon, w.issuer, []txnbuild.Operation{
		&txnbuild.Payment{Destination: wallet, Amount: "3", Asset: w.usdc},
	}, w.issuer)
	note("confirm (dave): %s/confirm#%s", base, linkToken(t, chat.ask(daveID, "dave", plain), base, "confirm"))

	if !until("dave sent his payment", func() bool {
		return balance(t, w.horizon, wallet, w.usdc) == "0.0000000"
	}) {
		return
	}
	note("reclaim (dave): %s/reclaim#%s", base, linkToken(t, chat.ask(daveID, "dave", "/reclaim"), base, "reclaim"))

	for time.Now().Before(deadline) && !released() {
		time.Sleep(2 * time.Second)
	}
}

// world is what the held state needs from the test that built it.
type world struct {
	db        *pgtest.DB
	horizon   *horizonclient.Client
	issuer    *keypair.Full
	usdc      txnbuild.CreditAsset
	treasury  string
	recipient string
	ada, ben  *keypair.Full
	eve       *keypair.Full
}

// age makes every workspace and every member in the database look as though it
// has been around for a while.
//
// The one place this test reaches past the server's own interfaces. The
// enrolment limiter refuses anyone first seen in the last ten minutes, in a
// workspace under an hour old; the test first proves it does, and then has no
// honest way to become older except to wait.
func age(t *testing.T, db *pgtest.DB) {
	t.Helper()
	for _, stmt := range []string{
		`UPDATE orgs SET created_at = now() - interval '2 hours'`,
		`UPDATE user_identities SET linked_at = now() - interval '1 hour'`,
	} {
		if _, err := db.Pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// youth undoes age: everybody and every workspace was first seen just now.
func youth(t *testing.T, db *pgtest.DB) {
	t.Helper()
	for _, stmt := range []string{
		`UPDATE orgs SET created_at = now()`,
		`UPDATE user_identities SET linked_at = now()`,
	} {
		if _, err := db.Pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// plus adds whole units to a balance as Horizon prints one.
func plus(t *testing.T, balance string, units int) string {
	t.Helper()
	whole, frac, _ := strings.Cut(balance, ".")
	n, err := strconv.Atoi(whole)
	if err != nil {
		t.Fatalf("read balance %q: %v", balance, err)
	}
	return strconv.Itoa(n+units) + "." + frac
}

// ---- The model: as much of a provider's API as a decoder calls.

// fakeModel answers a decode the way a careful model would: by pointing at
// tokens. It finds the verb, the first number and the address in the token
// list it was sent and returns their positions, copying the text exactly.
//
// It is deliberately dumb. Nothing downstream trusts the decoder, so a stand-in
// that can only point is enough to exercise everything that does the checking.
//
// It speaks both wire formats the server can be configured for: the OpenAI
// chat-completions one that most providers share, which is what this run uses
// unless E2E_DECODER says otherwise, and Anthropic's Messages API.
type fakeModel struct {
	*httptest.Server
	// down makes every request fail, the way a provider does when it is
	// rate-limiting, out of credit, or simply having a bad afternoon.
	down atomic.Bool
}

func newFakeModel(t *testing.T) *fakeModel {
	token := regexp.MustCompile(`(?m)^  \[(\d+)\] (.+)$`)

	// point reads the token list a decoder sent and answers with positions.
	point := func(listing string) string {
		span := func(i int, text string) map[string]any {
			return map[string]any{"text": text, "turn": 0, "start": i, "end": i + 1}
		}
		out := map[string]any{}
		for _, m := range token.FindAllStringSubmatch(listing, -1) {
			i, _ := strconv.Atoi(m[1])
			text := m[2]
			switch {
			case out["action"] == nil && strings.EqualFold(text, "send"):
				out["action"] = span(i, text)
			case out["amount"] == nil && regexp.MustCompile(`^\d`).MatchString(text):
				out["amount"] = span(i, text)
			case out["destination"] == nil && len(text) == 56 && strings.HasPrefix(text, "G"):
				out["destination"] = span(i, text)
				out["destination_kind"] = "address"
			}
		}
		if out["action"] == nil {
			out["action"] = map[string]any{"text": "", "turn": 0, "start": 0, "end": 0}
		}
		decoded, _ := json.Marshal(out)
		return string(decoded)
	}

	f := &fakeModel{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if f.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"the model is overloaded"}}`)
			return
		}

		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || len(req.Messages) == 0 {
				t.Errorf("the decoder sent something the stand-in cannot read: %s", raw)
				http.Error(w, `{"error":{"message":"unreadable"}}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl_e2e", "object": "chat.completion",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{
						"role": "assistant", "content": point(req.Messages[len(req.Messages)-1].Content),
					},
				}},
			})
			return
		}

		var req struct {
			Messages []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &req); err != nil || len(req.Messages) == 0 || len(req.Messages[0].Content) == 0 {
			t.Errorf("the decoder sent something the stand-in cannot read: %s", raw)
			http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"unreadable"}}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_e2e", "type": "message", "role": "assistant", "model": "claude-opus-5",
			"content":     []map[string]any{{"type": "text", "text": point(req.Messages[0].Content[0].Text)}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

// decoderEnv points the server at the stand-in, in whichever wire format this
// run is exercising.
func decoderEnv(model *fakeModel) map[string]string {
	if os.Getenv("E2E_DECODER") == "anthropic" {
		return map[string]string{
			"STELFIN_DECODER":    "anthropic",
			"ANTHROPIC_BASE_URL": model.URL,
			"ANTHROPIC_API_KEY":  "e2e-not-a-key",
		}
	}
	return map[string]string{
		"STELFIN_DECODER_BASE_URL": model.URL + "/v1",
		"STELFIN_DECODER_API_KEY":  "e2e-not-a-key",
		"STELFIN_DECODER_MODEL":    "e2e-model",
	}
}

// ---- Discord: interactions in, followups out.

// fakeDiscord is the part of Discord's REST API the transport calls, and the
// key Discord would sign its deliveries with.
type fakeDiscord struct {
	*httptest.Server
	public  ed25519.PublicKey
	private ed25519.PrivateKey

	mu       sync.Mutex
	replies  chan string
	commands []string
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate discord key: %v", err)
	}
	f := &fakeDiscord{public: public, private: private, replies: make(chan string, 32)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/applications/"+discordApp+"/commands":
			var cmds []struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &cmds)
			f.mu.Lock()
			for _, c := range cmds {
				f.commands = append(f.commands, c.Name)
			}
			f.mu.Unlock()
			io.WriteString(w, `[]`)

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/webhooks/"+discordApp+"/"):
			// The edit of a deferred, ephemeral response: the only route by
			// which a reply reaches the person who asked and nobody else.
			var m struct {
				Content string `json:"content"`
			}
			_ = json.Unmarshal(raw, &m)
			f.replies <- m.Content
			io.WriteString(w, `{"id":"1"}`)

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/channels/"):
			// A post to a channel is a post everyone in it can read.
			t.Errorf("a reply was posted to a Discord channel: %s", raw)
			io.WriteString(w, `{"id":"1"}`)

		default:
			http.Error(w, `{"message":"404: Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDiscord) published(t *testing.T) []string {
	t.Helper()
	names := settled(func() []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]string(nil), f.commands...)
	})
	if len(names) == 0 {
		t.Error("no command menu was published to Discord")
	}
	return names
}

func (f *fakeDiscord) next(t *testing.T) string {
	t.Helper()
	select {
	case text := <-f.replies:
		return text
	case <-time.After(90 * time.Second):
		t.Fatal("no reply reached Discord")
		return ""
	}
}

func (f *fakeDiscord) expectSilence(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case text := <-f.replies:
		t.Fatalf("expected no reply on Discord, got: %s", text)
	case <-time.After(wait):
	}
}

// server is a Discord server, and the people in it using slash commands.
type server struct {
	t       *testing.T
	base    string
	discord *fakeDiscord
	seq     int
}

func (g *server) ask(userID, name string, admin bool, command string, options map[string]string) string {
	g.t.Helper()
	g.say(userID, name, admin, command, options)
	return g.discord.next(g.t)
}

// say delivers one interaction, signed the way Discord signs: Ed25519 over the
// timestamp and the exact body.
func (g *server) say(userID, name string, admin bool, command string, options map[string]string) {
	g.t.Helper()
	g.seq++

	opts := []map[string]any{}
	for k, v := range options {
		kind := 3 // STRING
		if k == "user" {
			kind = 6 // USER: the value is the picked account's id
		}
		opts = append(opts, map[string]any{"name": k, "type": kind, "value": v})
	}
	permissions := "0"
	if admin {
		permissions = "8" // ADMINISTRATOR
	}
	body, _ := json.Marshal(map[string]any{
		"id": fmt.Sprintf("99000000000000%04d", g.seq), "type": 2,
		"token":    fmt.Sprintf("interaction-token-%d", g.seq),
		"guild_id": discordGuild, "channel_id": "550000000000000001",
		"data": map[string]any{"name": command, "options": opts},
		"member": map[string]any{
			"user":        map[string]any{"id": userID, "username": name},
			"roles":       []string{},
			"permissions": permissions,
		},
	})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ed25519.Sign(g.discord.private, append([]byte(ts), body...))

	deliver := func(signature string) (int, string) {
		req, _ := http.NewRequest("POST", g.base+"/webhook/discord", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature-Ed25519", signature)
		req.Header.Set("X-Signature-Timestamp", ts)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			g.t.Fatalf("deliver /%s: %v", command, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	// The same body under somebody else's signature is not from Discord.
	_, stranger, _ := ed25519.GenerateKey(nil)
	forged := ed25519.Sign(stranger, append([]byte(ts), body...))
	if code, _ := deliver(hex.EncodeToString(forged)); code != http.StatusForbidden {
		g.t.Fatalf("a forged Discord delivery of /%s answered %d, want 403", command, code)
	}

	code, ack := deliver(hex.EncodeToString(sig))
	if code != http.StatusOK {
		g.t.Fatalf("webhook answered %d to /%s", code, command)
	}
	// Deferred and ephemeral: acknowledged inside Discord's three seconds, and
	// visible to nobody but the person who asked.
	if !strings.Contains(ack, `"type":5`) || !strings.Contains(ack, `"flags":64`) {
		g.t.Fatalf("/%s was acknowledged with %s, want a deferred ephemeral response", command, ack)
	}
}

// throwawayServiceAccount is a Google service-account key file for an account
// that does not exist, with a key generated for this run.
func throwawayServiceAccount(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encode key: %v", err)
	}
	out, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"client_email":   "stelfin-e2e@stelfin-e2e.iam.gserviceaccount.com",
		"private_key_id": "e2e",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	return string(out)
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// statementIn reads the digest a challenge commits to out of its envelope, and
// fails if the envelope is anything other than an unsubmittable set of data
// entries — which is what a signer's page checks before offering it.
func statementIn(t *testing.T, envelope string) string {
	t.Helper()
	generic, err := txnbuild.TransactionFromXDR(envelope)
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	tx, ok := generic.Transaction()
	if !ok {
		t.Fatal("envelope is not a plain transaction")
	}
	if tx.SequenceNumber() != 0 {
		t.Fatalf("a grant envelope has sequence %d; it could be submitted", tx.SequenceNumber())
	}
	committed := ""
	for _, op := range tx.Operations() {
		data, ok := op.(*txnbuild.ManageData)
		if !ok {
			t.Fatalf("a grant envelope contains a %T", op)
		}
		if data.Name == "stelfin statement" {
			committed = string(data.Value)
		}
	}
	return committed
}

// deployAssetContract deploys the built-in contract of a classic asset, using
// stelfin's own settlement client to build, simulate and submit it.
func deployAssetContract(t *testing.T, issuer *keypair.Full, asset txnbuild.CreditAsset) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, err := settlement.New(settlement.Config{
		HorizonURL: horizonURL, NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}
	if client, err = client.DialSoroban(sorobanURL, nil); err != nil {
		t.Fatalf("dial soroban: %v", err)
	}

	xdrAsset, err := asset.ToXDR()
	if err != nil {
		t.Fatalf("asset: %v", err)
	}
	built, err := client.Build(ctx, settlement.BuildRequest{
		Source: issuer.Address(),
		Operations: []txnbuild.Operation{&txnbuild.InvokeHostFunction{
			HostFunction: xdr.HostFunction{
				Type: xdr.HostFunctionTypeHostFunctionTypeCreateContract,
				CreateContract: &xdr.CreateContractArgs{
					ContractIdPreimage: xdr.ContractIdPreimage{
						Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAsset, FromAsset: &xdrAsset,
					},
					Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset},
				},
			},
			SourceAccount: issuer.Address(),
		}},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("build deploy: %v", err)
	}
	sim, err := client.Simulate(ctx, built)
	if err != nil {
		t.Fatalf("simulate deploy: %v", err)
	}
	assembled, err := client.Assemble(built, sim)
	if err != nil {
		t.Fatalf("assemble deploy: %v", err)
	}
	signed, err := assembled.Sign(network.TestNetworkPassphrase, issuer)
	if err != nil {
		t.Fatalf("sign deploy: %v", err)
	}
	if _, err := client.SubmitSoroban(ctx, signed); err != nil {
		t.Fatalf("submit deploy: %v", err)
	}

	id, err := xdrAsset.ContractID(network.TestNetworkPassphrase)
	if err != nil {
		t.Fatalf("contract id: %v", err)
	}
	address, err := strkey.Encode(strkey.VersionByteContract, id[:])
	if err != nil {
		t.Fatalf("encode contract id: %v", err)
	}
	return address
}

// post sends a JSON-RPC request the way an MCP client would.
func post(t *testing.T, url, bearer, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ---- The group: people typing.

type group struct {
	t        *testing.T
	base     string
	telegram *fakeTelegram
	mu       sync.Mutex
	update   int64
}

func (g *group) nextUpdate() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.update++
	return 1000 + g.update
}

// ask sends a message and returns the bot's private reply to its author.
func (g *group) ask(userID int64, handle, text string) string {
	g.t.Helper()
	g.say(userID, handle, text)
	return g.telegram.next(g.t, userID)
}

func (g *group) say(userID int64, handle, text string) {
	g.t.Helper()
	if code := g.deliver(userID, handle, text, webhookSecret, 0); code != http.StatusOK {
		g.t.Fatalf("webhook answered %d to %q", code, text)
	}
}

// overheard posts words that were not said to the bot: two people talking in
// a group it happens to be in.
func (g *group) overheard(userID int64, handle, text string) {
	g.t.Helper()
	if code := g.post(userID, handle, text, nil, webhookSecret, 0); code != http.StatusOK {
		g.t.Fatalf("webhook answered %d to %q", code, text)
	}
}

// deliver posts one update the way Telegram would, annotating the leading
// command by Telegram's own rule: letters, digits and underscores only.
//
// Words with no command are said to the bot the way a person in a group says
// them, by opening with its name. Without that they are other people's
// conversation; see overheard.
func (g *group) deliver(userID int64, handle, text, secret string, updateID int64) int {
	g.t.Helper()
	var entities []map[string]any
	if cmd := regexp.MustCompile(`^/[A-Za-z0-9_]+`).FindString(text); cmd != "" {
		entities = []map[string]any{{"type": "bot_command", "offset": 0, "length": len(cmd)}}
	} else {
		at := "@" + botUsername
		text = at + " " + text
		entities = []map[string]any{{"type": "mention", "offset": 0, "length": len(at)}}
	}
	return g.post(userID, handle, text, entities, secret, updateID)
}

func (g *group) post(
	userID int64, handle, text string, entities []map[string]any, secret string, updateID int64,
) int {
	g.t.Helper()
	if updateID == 0 {
		updateID = g.nextUpdate()
	}
	message := map[string]any{
		"message_id": updateID,
		"from":       map[string]any{"id": userID, "is_bot": false, "username": handle},
		"chat":       map[string]any{"id": groupID, "type": "supergroup"},
		"text":       text,
	}
	if entities != nil {
		message["entities"] = entities
	}
	body, _ := json.Marshal(map[string]any{"update_id": updateID, "message": message})

	req, _ := http.NewRequest("POST", g.base+"/webhook/telegram", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("deliver %q: %v", text, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// ---- Telegram's Bot API, as much of it as the transport calls.

type fakeTelegram struct {
	*httptest.Server
	mu       sync.Mutex
	inbox    map[int64]chan string
	commands []string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{inbox: make(map[int64]chan string)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/bot" + botToken + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		switch strings.TrimPrefix(r.URL.Path, prefix) {
		case "getMe":
			fmt.Fprintf(w, `{"ok":true,"result":{"id":%d,"is_bot":true,"username":%q}}`, botID, botUsername)
		case "setWebhook":
			// Never called here: the server is on localhost, where Telegram
			// could not reach it, and it knows not to ask.
			t.Errorf("the server pointed Telegram at a webhook it cannot reach: %s", raw)
			io.WriteString(w, `{"ok":true,"result":true}`)
		case "sendMessage":
			var m struct {
				ChatID string `json:"chat_id"`
				Text   string `json:"text"`
			}
			_ = json.Unmarshal(raw, &m)
			var id int64
			fmt.Sscan(m.ChatID, &id)
			if id == groupID {
				// Every reply this bot sends is private. One landing in the
				// group is a link several hundred people could tap.
				t.Errorf("a reply was posted to the group: %s", m.Text)
			}
			f.box(id) <- m.Text
			io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		case "getChatMember":
			var m struct {
				UserID string `json:"user_id"`
			}
			_ = json.Unmarshal(raw, &m)
			status := "member"
			if m.UserID == fmt.Sprint(adaID) {
				status = "creator"
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"status":%q}}`, status)
		case "setMyCommands":
			var m struct {
				Commands []struct {
					Command string `json:"command"`
				} `json:"commands"`
			}
			_ = json.Unmarshal(raw, &m)
			f.mu.Lock()
			for _, c := range m.Commands {
				f.commands = append(f.commands, c.Command)
			}
			f.mu.Unlock()
			io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegram) box(id int64) chan string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inbox[id] == nil {
		f.inbox[id] = make(chan string, 32)
	}
	return f.inbox[id]
}

func (f *fakeTelegram) next(t *testing.T, userID int64) string {
	t.Helper()
	select {
	case text := <-f.box(userID):
		return text
	case <-time.After(90 * time.Second):
		t.Fatalf("no reply reached user %d", userID)
		return ""
	}
}

func (f *fakeTelegram) expectSilence(t *testing.T, userID int64, wait time.Duration) {
	t.Helper()
	select {
	case text := <-f.box(userID):
		t.Fatalf("expected no reply to user %d, got: %s", userID, text)
	case <-time.After(wait):
	}
}

func (f *fakeTelegram) published(t *testing.T) []string {
	t.Helper()
	names := settled(func() []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]string(nil), f.commands...)
	})
	if len(names) == 0 {
		t.Error("no command menu was published to Telegram")
	}
	return names
}

// settled waits for a list the server fills in after it has started to stop
// growing, and returns it.
//
// The platforms are told their commands beside the server coming up, not
// before it, so that somebody else's API being slow does not hold up this one.
// A test that looked the moment the server answered would be racing that.
func settled(read func() []string) []string {
	deadline := time.Now().Add(30 * time.Second)
	last, since := -1, time.Now()
	for time.Now().Before(deadline) {
		n := len(read())
		if n != last {
			last, since = n, time.Now()
		} else if n > 0 && time.Since(since) > time.Second {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return read()
}

// ---- The server process.

func startServer(t *testing.T, base string, env map[string]string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stelfind")
	build := exec.Command("go", "build", "-o", bin, "../cmd/stelfind")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stelfind: %v\n%s", err, out)
	}

	logs := &bytes.Buffer{}
	cmd := exec.Command(bin)
	// A clean environment: nothing from the developer's shell or .env leaks in,
	// so the test proves the documented variables are sufficient.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = logs, logs
	// When the state is being held for a browser, whoever is walking it wants
	// the server's side of anything that goes wrong while they do — and by
	// then this test has passed, so nothing below would print it.
	if path := os.Getenv("E2E_HOLD_FILE"); path != "" && os.Getenv("E2E_HOLD") != "" {
		if f, err := os.Create(path + ".server.log"); err == nil {
			t.Cleanup(func() { f.Close() })
			cmd.Stdout, cmd.Stderr = io.MultiWriter(logs, f), io.MultiWriter(logs, f)
		}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stelfind: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
		}
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("stelfind never became healthy:\n%s", logs.String())
}

// ---- The browser: the JSON the signing pages exchange.

func call(t *testing.T, method, url, token string, body any, want int, out any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, url, resp.StatusCode, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, url, raw, err)
		}
	}
}

// linkToken pulls the fragment-borne token out of a reply's link.
func linkToken(t *testing.T, reply, base, page string) string {
	t.Helper()
	prefix := base + "/" + page + "#"
	at := strings.Index(reply, prefix)
	if at < 0 {
		t.Fatalf("reply carries no %s link:\n%s", page, reply)
	}
	return strings.Fields(reply[at+len(prefix):])[0]
}

// sign adds signatures to an envelope, as a wallet would.
func sign(t *testing.T, envelope string, keys ...*keypair.Full) string {
	t.Helper()
	generic, err := txnbuild.TransactionFromXDR(envelope)
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	tx, ok := generic.Transaction()
	if !ok {
		t.Fatal("envelope is not a plain transaction")
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, keys...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	out, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("expected %q in:\n%s", want, text)
	}
}

// ---- Testnet.

func funded(t *testing.T, h *horizonclient.Client, name string) *keypair.Full {
	t.Helper()
	kp := keypair.MustRandom()
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if _, err = h.Fund(kp.Address()); err == nil {
			t.Logf("funded %s %s", name, kp.Address())
			return kp
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("friendbot fund %s: %v", name, err)
	return nil
}

func submit(
	t *testing.T, h *horizonclient.Client, source *keypair.Full,
	ops []txnbuild.Operation, signers ...*keypair.Full,
) {
	t.Helper()
	account, err := h.AccountDetail(horizonclient.AccountRequest{AccountID: source.Address()})
	if err != nil {
		t.Fatalf("load %s: %v", source.Address(), err)
	}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &account,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              txnbuild.MinBaseFee * 100,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	tx, err = tx.Sign(network.TestNetworkPassphrase, signers...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := h.SubmitTransaction(tx); err != nil {
		if herr := horizonclient.GetError(err); herr != nil {
			codes, _ := herr.ResultCodes()
			t.Fatalf("submit: %v %+v", err, codes)
		}
		t.Fatalf("submit: %v", err)
	}
}

func balance(t *testing.T, h *horizonclient.Client, address string, asset txnbuild.CreditAsset) string {
	t.Helper()
	account, err := h.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		t.Fatalf("load %s: %v", address, err)
	}
	for _, b := range account.Balances {
		if b.Code == asset.Code && b.Issuer == asset.Issuer {
			return b.Balance
		}
	}
	t.Fatalf("%s holds no %s trustline", address, asset.Code)
	return ""
}

func freePort(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}
