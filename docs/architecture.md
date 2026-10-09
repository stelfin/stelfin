# stelfin — architecture plan

## Context

stelfin is a non-custodial bot a DAO runs its Stellar treasury through, from
Telegram and Discord, with a connector layer so the other tools a community
touches on-chain are reachable from the same place.

This document is the plan being built from, and the record of what building
it found. It replaces the plan for the WhatsApp wallet stelfin began as — that
document described a zkLogin/Groth16 authentication model and a Soroban custom
account that were never built and are not in scope. Nothing of it is preserved
except the parts that turned out to be right about money, which are recorded in
the design record instead.

## Settled decisions

| Decision | Choice | Rationale |
|---|---|---|
| Chain | Stellar | native M-of-N multisig, sponsored reserves, fee-bump, SEP standards, ~5s finality, no reorgs |
| Server language | Go | `stellar/go-stellar-sdk` is first-party and ships both Horizon and Soroban RPC clients; Go is the payments-infra norm |
| Contracts | Rust / Soroban | one memory-safe language for everything key-adjacent |
| Custody | Non-custodial, adaptive | the bot builds and routes; the DAO's existing signing authority decides |
| Platforms | Telegram **and** Discord | one channel-agnostic core, two transports, from the start |
| Tenancy | Multi-tenant | one deployment, many DAOs; `(channel, space_id)` → exactly one org |
| Identity | SEP-10 + an identity table | one human, many handles, one proved address |
| Member wallets | Connect an existing address, or be provisioned | provisioning is rate-limited and off by default |
| Commands | Slash commands primary, free text as fallback | the existing decoder is kept, and both paths converge on the same verified instruction |
| Ledger role | Index of on-chain state | not authoritative for balances |
| Network | Mainnet-capable, hard-gated | the operator key in an environment variable refuses to start on the public network without an explicit acknowledgement |
| Web | Static pages served from the binary | nothing in the signing path has a build step |

## Target architecture

```
Telegram ─┐                                   ┌─ Horizon  (classic)
          ├─ chat.Transport ─ core ─ settlement┤
Discord  ─┘        │           │              └─ Soroban RPC (contracts)
                   │           ├─ ledger  (Postgres, double-entry, org-scoped)
                   │           ├─ identity (SEP-10, roles)
                   │           ├─ signer   (operator / external)
                   │           └─ connector (Sheets, MCP client)
                   │
                   └─ web  ── the browser that verifies and signs
                              (confirm · enroll · link · approve · reclaim · call)

mcpserver ── read-only and propose-only tools, for agents outside
contracts ── dao_treasury · connector_registry  (Rust, one Cargo workspace)
```

Package layout:

| Path | What it is |
|---|---|
| `chat/` | the platform-agnostic contract: `Actor`, `Conversation`, `Inbound`, `Reply`, `Transport`, `Registry` |
| `chat/chattest/` | the fake transport and the conformance suite every transport must pass |
| `internal/telegram`, `internal/discord` | the two transports |
| `core/` | orchestration: org resolution, role checks, command dispatch, the free-text fallback |
| `api/` | HTTP only: webhooks, the signing endpoints, the token types |
| `intent/`, `decoder/` | tokenizer, verifier, normalizer, resolver; the Claude decoder |
| `identity/` | SEP-10 challenges, identity linking, roles |
| `treasury/` | treasuries, signer sets, proposals |
| `signer/` | the signing seam and its implementations |
| `connector/` | the connector model, guard, and adapters |
| `mcpserver/` | stelfin as an MCP server |
| `ledger/`, `ledger/store/` | the double-entry engine, and every query in the system |
| `settlement/` | transaction construction, description, submission — classic and Soroban |
| `ingestion/` | Horizon and Soroban event ingestion |
| `contracts/` | the Rust workspace |
| `web/` | the pages that verify and sign |
| `marketing/` | the public site (a separate Next.js module) |

## The seams

**Transport.** `Verify(*http.Request) ([]byte, error)` → `Parse([]byte)
(Delivery, error)` → `Send`. Ordered by trust: nothing is parsed that was not
authenticated, nothing acted on that was not parsed. `Delivery.Ack` is computed
from the verified body and written before any work — Discord declares an
interaction failed after three seconds, and Telegram retries a slow response,
which would start one payment flow twice.

Verification differs in kind between the two platforms and the difference is
recorded rather than smoothed over: Discord signs `timestamp || rawBody` with
Ed25519; Telegram echoes a shared secret in a header, which proves the caller
knew the secret and says nothing about the body. On Telegram, therefore, nothing
inside the body may establish privilege — admin status is fetched from the API,
not read from the update.

**Reply.** Every outbound message suppresses link previews unconditionally, and
the registry refuses to deliver a reply containing an authority link unless it
is private to the actor. Telegram has no ephemeral messages in groups, so
"private" there means a DM — which requires the member to have started a chat
with the bot, and the setup flow has to walk them through that or the first
payout blocks on a dead end.

**Identity.** `(channel, channel_user_id)` → member; member → one address, proved
by a SEP-10 challenge signed in the browser. The challenge is built with
sequence zero and is structurally unsubmittable. Proving control of an M-of-N
treasury uses `VerifyChallengeTxThreshold`, so it takes the same signatures a
payment would.

**Signing.** One interface: the operator's key, and an *external* signer that
contributes nothing and reports how much weight is still needed. The router has
no branch that returns a key-holding signer for a DAO treasury. A treasury held
by a contract does not come through it at all — nobody collects signatures for
one; see "Treasuries held by a contract" below.

**Description.** The server sends a canonical description of a transaction and
the browser re-derives the same description from the XDR, comparing bytes. The
shape rules — "an enrolment is exactly these four operations in this order" —
live in the browser, not in what the server sends, because the server is the
thing being checked.

## Data model

Beyond the existing double-entry core:

- `orgs` — one per DAO, plus one `platform` row for the operator's own float,
  reserves and fees. Carries policy: spend ceilings, proposal TTL, enrolment
  budget (zero by default).
- `org_spaces` — `(channel, space_id)` primary key. The tenant lookup.
- `members`, `user_identities`, `member_roles`, `role_bindings` — one human, many
  handles, one proved address. Composite foreign keys `(member_id, org_id)` make
  tenant containment a database property.
- `org_treasuries`, `treasury_signers` — the DAO's accounts and their signer
  sets. The signer set is a **cache, never an authority**; it is refreshed from
  Horizon when a proposal opens and again before submission, and if it is stale
  the bot's "two more signatures needed" message is wrong while the network's
  answer is still right.
- `proposals`, `proposal_signatures`, `proposal_events` — the base envelope is
  immutable and unsigned; signatures are rows, and the submittable envelope is
  rebuilt from them each time. Read-modify-write on a stored envelope loses one
  of two simultaneous approvals.
- `tracked_addresses` — one address, one ledger account, so an incoming payment
  posts to exactly one place.
- `connectors`, `connector_grants`, `drafts` — the connector layer. A
  connector's address and token are sealed against `(org, name)`, so a row moved
  between orgs decrypts to nothing. A grant is in force only with the signed
  envelope that put it there, which the table itself requires.

## Contracts

One Cargo workspace at `contracts/`, two contracts.

**`dao_treasury`** merges treasury policy and proposals/voting. They are one
state machine over one pot of money; splitting them buys a cross-contract
authorization dance and a window in which each disagrees about whether a
proposal passed. Members with weights, quorum and approval thresholds in basis
points, a voting window, a timelock, a rolling-window auto-spend limit, and a
recipient allowlist.

It has no admin. All of that is set once, by the constructor, in the
transaction that creates the contract, and changed afterwards only by a
proposal: a payment, the policy, a member's weight and the allowlist are the
same kind of thing, decided the same way. A proposal is settled as soon as the
votes not yet cast could not change it, and the timelock then runs in full.
Two actions skip the timelock because they can only stop something — removing a
recipient and revoking another proposal — and they are how members use it.

`execute` deliberately takes no authorization: anyone may execute a proposal
that has passed and cleared its timelock. Restricting it to the bot would make
our uptime a treasury liveness dependency, which is the worst property a
treasury tool can have.

### Treasuries held by a contract

A workspace's treasury is one of two kinds, and they are signed for in
different ways. A classic account is one envelope that enough of its signers
sign. A `dao_treasury` has no envelope to pass round: each member makes their
own call to it — propose, vote, withdraw, spend, deposit — from their own
wallet, and the contract counts.

So the server collects nothing and decides nothing. For each command it builds
the one call that member asked for, with their address as its source; proves by
simulation that the contract would take it, so that what the contract refuses is
refused before anybody signs and with the contract's own reason; records it by
its hash; and hands that member a link. The page (`/call`) derives the
description from the envelope like every other, then reads the call against the
contract's interface, which is pinned in the page, and says in words what it
does — or refuses, for any call other than one a member makes to this treasury
from this wallet. The member signs; the server wraps the signed call in a fee
bump the operator pays, and sends it. A call is accepted only if it hashes to
one this server built for whoever is submitting it, and is sent once.

The server holds one key in this and it is not a member's. A *relay* account —
the operator's, unless `STELFIN_RELAY_SEED` names another — sends the calls
nobody has to sign for: creating a treasury, and carrying out a proposal the
members have passed. The contract gives it no authority; it takes `execute`
from anybody.

Nothing about a treasury is cached. Who its voters are, what it may pay, and
whether a proposal passed are read from the contract each time they are needed,
by simulation. The one thing stored is a proposer's note, and it is shown only
when it hashes to the commitment the contract holds for that proposal.

What a page cannot check is said to be unchecked. A vote names a proposal by
its number; what that proposal does is on the ledger and not in the transaction
being signed. The page shows what stelfin says about it in a box of its own,
and nothing the page asserts depends on it.

Three programs have to agree about how the contract's values look on the wire:
the contract, the Go that builds calls (`contracts/treasuryabi`), and the page
that reads them (`web/static/treasury.js`). Each is tested against one file,
`contracts/dao-treasury/testdata/abi.tsv`, which the contract's own suite
writes from its real types.

**Lifetime.** Ledger entries are rented by the byte and archived when the rent
runs out; on testnet a new entry gets seven days. A treasury renews its own
small state as it is used. It does not renew its code, which is tens of
kilobytes shared by every treasury: doing that inside a member's call put a
thirty-five XLM bid in the transaction that creates a treasury. The server
keeps the code and each treasury's instance alive from outside instead — at
start and every six hours, topping up whatever has under three days left, a
little at a time, with a ceiling of two XLM on any one extension. What it
spends is logged. A deployment that stops running stops paying, and its code is
archived within the week; uploading it again brings it back.

**`connector_registry`** holds capability grants — connector id, capabilities, a
digest of the approved surface, limits, a validity window, and revocation as a
positive fact. It never holds a credential or a bearer URL, because everything
in a contract is public; it stores a salted hash of the endpoint and the
endpoint itself lives off-chain.

Deployment is proved rather than claimed. `contracts/deployments.json` commits
the WASM hash and upload transaction per network; a hermetic test asserts the
rebuilt WASM matches the committed source hash, and an integration test reads
the on-chain contract-code entry — it cannot pass unless the upload actually
happened.

## Build sequence

Every phase ends with `make check` green and something demonstrable.

| Phase | Contents |
|---|---|
| 0 | Excise WhatsApp; introduce `chat/` and the conformance suite; route `POST /webhook/{channel}`; rewrite the docs; remove the dead marketing CTA |
| 1 | Telegram transport |
| 2 | Discord transport |
| 3 | Multi-tenant schema (fresh migration set) and `ledger/store` |
| 4 | Identity, SEP-10, roles, the enrolment limiter |
| 5 | Generalised transaction description and browser verification |
| 6 | Classic operations, treasuries, proposals, the signer seam |
| 7 | Soroban RPC |
| 8 | The contracts, deployed and pinned |
| 9 | Ingestion: Horizon operations, Soroban events, SAC transfers |
| 10 | Connectors and the Sheets adapter |
| 11 | MCP server with read and propose tiers; MCP client, reachable as a connector |
| 12 | Mainnet rails; the marketing rebuild; the generic webhook connector |

## Verification

- `make check` (fmt, vet, test) at every phase boundary; `make test-race` and
  `make fuzz-money` before a deploy. Tests bring their own Postgres, so none of
  this needs Docker or a local database.
- A cross-language golden corpus for transaction description: the same bytes
  asserted by a Go test and a Node test. Without it the two implementations
  drift and the guarantee disappears silently.
- `make e2e`: the end-to-end bar below, run against live testnet with the chat
  platform and the model replaced by local stand-ins and everything else real —
  the `stelfind` binary, Postgres, Horizon. `make e2e-browser` is the same run
  with every signing page then walked in headless Chrome, because the Go half
  plays the browser by calling the JSON endpoints and so proves nothing about
  the pages. What these two found is under *Found by running it end to end*.
- `go test -tags=integration ./settlement/` against live testnet and friendbot,
  creating its own dependencies rather than relying on a third party's account.
- `cargo test` including negative-authorization cases run *without*
  `mock_all_auths` — a contract suite that only runs under mocked auth proves
  nothing about authorization.
- The end-to-end bar: a stranger adds the bot to their own Discord server and
  Telegram group, runs setup, links a wallet by signature, links a treasury,
  proposes a payment, approves it in the browser, and watches it land on
  testnet — recognised as the same member on both platforms.

## What is left

Phases 0 to 10 and the mainnet rails are built, and the path from a chat message
to money on chain is exercised end to end on both platforms. What follows is
everything that is not, written down so it survives the session it was decided
in.

### Connectors: decided, built, and where they differ from the plan

Connectors are reachable from chat: registered by an admin, set up on a page,
granted by the treasury's signers, read through `connector.Guard`, and turned
into payroll drafts a proposer puts to the signers.

**The grant is not on chain, and that is a decision, not a gap.** The plan put
it in `connector_registry`. What was built instead is a statement in plain text
— connector, kind, host, endpoint digest, pinned surface, capabilities, rate —
whose SHA-256 rides in a SEP-10 challenge the treasury's signers sign at the
weight a payment takes. The signed envelope is kept with the grant
(`connector_grants.signed_xdr`), so the authorisation can be re-verified by
anyone holding the row, without this deployment.

The reasoning: the guard that enforces a grant is this deployment's code
wherever the grant is stored, so on-chain storage adds a public record and not a
control; a connector can only read and draft, so a bad grant costs noise and not
funds; and the on-chain path needed a contract instance per DAO, rent that must
be kept paid, a slot in the one-open-proposal queue, and the still-undesigned
page for confirming a contract call. What it gives up is the public audit trail.
The stored envelope can be mirrored to `connector_registry` later if that is
wanted; nothing about the grant format would change.

The browser checks what it is asked to sign. `link.js` hashes the statement it
is shown and refuses unless the envelope commits to exactly that digest — and
refuses any envelope that commits to a statement it is not showing, whatever the
server labels it.

**What each piece still does not do**, written where it lives and repeated here:

- `mcpclient` has been run against stelfin's own MCP server
  (`TestStelfinsMCPClientReadsStelfinsMCPServer`) and never against a third
  party's. It refuses a paginated `tools/list`, ends a call on any
  server-originated request including a ping, and does not pin `annotations` or
  `outputSchema`.
- `egress` refuses NAT64, 6to4 and Teredo wholesale rather than decoding the
  IPv4 address inside, so a deployment on an IPv6-only network with DNS64 could
  not reach IPv4-only endpoints.
- The Sheets exchange has never met Google's real token endpoint with a real
  key. `make e2e` gets as far as Google refusing a throwaway one.

**Agents drafting payments (the propose tier)** is built, on a decision made
here: a propose-tier token can write a draft and nothing more, the draft is
marked as the agent's everywhere it is shown, and its destinations must be
addresses — a saved name belongs to the member who saved it. A member proposes
it; the signers sign that.

### Contract calls

Built, behind three rules. `STELFIN_CONTRACT_CALLS` is off by default, because a
contract call moves money in ways the spend ceilings cannot see. Only contracts
on the workspace's allowlist, and only while they run the code they were
allowed with — the pin (a WASM hash, or the fact of being an asset's own
contract) is checked when the call is built and again before it is submitted.
And a call is a proposal like any other, described from its own envelope and
executed only at the treasury's spending weight. `make e2e` deploys the test
asset's own contract and pays through it.

A call that finds its state archived is refused, and the restore is offered as
a proposal of its own (`/restore`): it costs money and is not folded quietly
into the call. Restores are described by both renderers and pinned in the
golden corpus; none has been run on chain, because a test cannot make testnet
archive something on demand.

`ExtendFootprintTtl` is still not assembled. Nothing asks for it: extending is
paying ahead of time to avoid a restore, and restoring when it is needed is
built.

### Smaller things, each reachable today

- **Limits are read without a lock.** Two proposals racing can together exceed a
  daily ceiling by one payment. Every proposal still needs its signatures; the
  alternative is serialising every proposal in the deployment behind one row.
- **A linked wallet is booked in one workspace.** It has its own ledger account
  beside any provisioned one, so ingestion records its payments — in the first
  workspace that linked it. The network has one history and each payment is
  posted once.
- **Spend ceilings do not bound contract calls.** Stated where calls are turned
  on and in every `/invoke` reply.

### Found by running it end to end

Each of these passed every unit test. They are fixed; they are listed because
the way they were found says where the next ones are.

- **The enrolment limiter was written and never called.** `GrantEnrollment` —
  the budget, the daily caps, the float floor, one account per member — had no
  caller, and neither did `SetEnrollmentPolicy`. As wired, anybody who added the
  bot to a group could have the operator pay account reserves without limit. It
  is now taken in `SubmitEnrollment` before the operator's signature goes on,
  provisioning is off until an admin of a workspace with a proved treasury runs
  `/provisioning on`, and a chat message says so rather than a link that leads
  to a refusal.
- **A provisioned account could never be handed back.** It holds no XLM by
  design, and the hand-back transaction asked it to pay its own fee. `/reclaim`
  also looked for the wallet in the wrong place — at the address a member proved
  with `/link`, not the one provisioned for them. The operator now fee-bumps a
  hand-back as it does a payment.
- **Four of the five signing pages threw before rendering**, on
  `new StellarSdk.TransactionBuilder.fromXDR(…)`, and the enroll page then
  refused every provisioning transaction for a starting balance of
  `"0.0000000"` where it expected `"0"`. `web/static/*.test.js` runs the modules
  under Node and never loads a page. `make e2e-browser` now does.
- **`/pay`, `/link-treasury` and `/link-code` could not be invoked from
  Telegram**, which has no structured options and no hyphens in command names.
- **Nothing could grant a role after `/setup`**, so only whoever installed the
  bot could ever be offered a link.
- **Every payment logged a tamper warning**, by comparing the hash the user
  signed against the hash of the fee-bump around it.
- **A command that failed internally answered with silence.**

Found in the second pass, the same way:

- **`/send` handed the decoder a sentence with no verb in it**, so the one way
  to write a sentence on Discord could never be read as a payment.
- **A member with a wallet of their own met a red dead end** on the confirm
  page, which treated "the key is not on this device" as an error and not as an
  ordinary way to hold a wallet.
- **Nothing could attach a second chat space, save a recipient, set a limit,
  mint an agent token or suspend a workspace**, though the store had a method
  for each.
- **The connector layer was written and unreachable**: the guard, the sealer,
  the Sheets reader and batch payroll were tested packages that nothing in the
  running application constructed.

What end-to-end still does not cover:

- **A live model.** `make e2e` stands in for the Messages API with something
  that can only point at tokens. That exercises everything that checks the
  decoder and nothing about the decoder's own judgement.
- **The real platforms.** Telegram's Bot API and Discord's interactions are
  both stood in for, faithfully to their documented shapes, and neither has
  been driven against the real service from this tree.

### Smaller gaps, each stated where it lives

- `connector`'s rate limiter is in-process, so two instances of stelfin each
  allow the full rate. Making it shared costs a round trip to Postgres on the
  hot path of every connector call.
- `settlement.Assemble` refuses `ExtendFootprintTtl` and `RestoreFootprint`
  rather than assembling them. Neither is reachable from chat yet.
- Restoring archived Soroban state is surfaced (`ErrArchivedState`) and never
  done automatically, because restoring costs money and changes what the
  transaction does.

### Found by reading the paths a stranger walks

A third pass, after the two above, made by following what somebody who has never
seen the bot would actually do. Fixed; listed for the same reason as the others.

- **A treasury could be locked out of proposing.** One open proposal per
  account, no cancel, an expiry sweep that was never started, and a transaction
  the network included and failed left its proposal open for good. `/cancel`
  now exists, expiry happens when a proposal is created, read and on a timer,
  and a refusal from the network closes the proposal or leaves it open
  according to whether the envelope is spent.
- **First contact was silence.** A direct message is an unregistered space, and
  it is where the bot sends people; it ignored the Start it had asked for. A
  command before `/setup` got nothing, which on Discord is a spinner that never
  stops. The person is now answered and the room still is not.
- **Every message in a group was read as a payment.** The documentation said to
  turn Telegram's privacy mode off. Words now count only when they are said to
  the bot, and the transport registers its own webhook at start.
- **A handler that panicked took the server down**, and one that failed before
  answering answered nothing.
- **Nine commands could only refuse** on a deployment with the defaults, and
  replies over a platform's length limit were refused whole.
- **Setting a wallet up could strand a browser.** The key was stored before the
  wallet existed, so any failed attempt left a key that made every later one
  stop at "this device already has a wallet". The limiter's age rule was met
  only after a link had been handed out.
- **A signing page could not be reloaded**, an empty paste took the form away on
  two of the five, and nothing offered to copy the envelope a signer has to
  carry to their wallet.

### What is next

Nothing on this list is waiting to be built. What is left is what no test here
can be: the bot driven from a real Telegram group and a real Discord server,
with real wallets, by people who did not write it.

Built since this list was first written, and no longer on it: signing with
Freighter beside paste; a decoder for any provider speaking the OpenAI format,
with a fixed grammar behind it; `/balance`; treasury trustlines and swaps as
proposals; and treasuries held by a contract its members vote in.

Three things about contract treasuries are limits rather than gaps, and are
said here so that nobody finds them out. Only assets this deployment can name —
XLM and the one it is configured with — are shown in such a treasury's balance:
a contract has no list of what it holds, so each token has to be asked, and a
token nobody asked about is not shown as zero, it is not shown. A treasury that
is archived for want of rent is brought back by the first call to it, at that
call's expense, which may be more than the ceiling the server will bid. And the
events the contract publishes are not read into the ledger here: a contract
treasury's history is on the network, and `/proposals` reads it from there.

Three things about swaps are decisions rather than gaps. A swap is a path
payment the treasury makes to itself, and nothing else is called one. It has no
default floor: asked without one, the bot answers with what the market offers
and proposes nothing, because the floor is the whole of a swap's risk and a
default would be a number chosen here about somebody else's money. And it is
checked against the market again just before it is submitted, since a swap that
cannot meet its floor fails on the ledger and takes its signatures with it; one
that would is held, not sent.

### Not code

- **`DESIGN.md` is gitignored.** Nothing tracked links to it any more; it is the
  owner's to publish or keep.
- **No chat transport is registered on the deployment** until its Telegram and
  Discord variables are set. `POST /webhook/{any}` returns 404 until then, and
  the server now refuses to start with a platform configured and no web-auth
  key, since such a bot could be spoken to and could finish nothing.
- **The deployment sleeps.** On the free plan it stops after fifteen idle
  minutes and takes about thirteen seconds to wake, which Telegram tolerates and
  Discord's three-second limit does not. An external ping to `/health` keeps it
  up; nothing in the code can.
- **Testnet is reset from time to time**, which removes every account and
  contract. Recovery is a fresh database, a re-funded operator, and the
  contract uploads again.

### Roadmap — decided against, for now

These were settled as roadmap items when the rebuild was scoped, not as
oversights:

- Bot-provisioned DAO treasuries (provisioning one from chat).
- Opt-in custodial mode.
- An AI-model connector.
- A trading and DEX connector.
- Per-org Google OAuth, rather than one service account.
- A KMS signer. The `signer` seam exists and takes a signing function, so this
  is configuration rather than redesign.

## Not yet designed

- Confirming an arbitrary Soroban contract call honestly in a browser that
  cannot, in general, re-derive what the call will do. Done for one contract,
  `dao_treasury`, whose interface the page pins; a third-party contract called
  with `/invoke` is still shown value by value and not interpreted.
- Concurrent proposals against one treasury (CAP-21 preconditions).
- A remote signer for the operator key.
- Compliance: Travel Rule, sanctions screening, SEP-12 KYC.
- Self-hosted Horizon and Soroban RPC rather than the public instances.
