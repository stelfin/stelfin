# stelfin — architecture plan

## Context

stelfin is a non-custodial bot a DAO runs its Stellar treasury through, from
Telegram and Discord, with a connector layer so the other tools a community
touches on-chain are reachable from the same place.

This document is the plan being built from. `../DESIGN.md` is the reasoning
behind it. It replaces the plan for the WhatsApp wallet stelfin began as — that
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
                   │           ├─ signer   (operator / external / smart account)
                   │           └─ connector (Sheets, MCP client)
                   │
                   └─ web  ── the browser that verifies and signs
                              (confirm · enroll · link · approve)

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

**Signing.** One interface, four implementations: the operator's key (local, or
remote), an *external* signer that contributes nothing and reports how much
weight is still needed, and a smart-account signer whose completeness comes from
simulating `__check_auth`. The router has no branch that returns a key-holding
signer for a DAO treasury.

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
- `connectors`, `connector_credentials`, `connector_grants_cache`,
  `connector_calls`, `drafts` — the connector layer, with credentials sealed
  against `(org, connector, purpose)` so a row moved between orgs decrypts to
  nothing.

## Contracts

One Cargo workspace at `contracts/`, two contracts.

**`dao_treasury`** merges treasury policy and proposals/voting. They are one
state machine over one pot of money; splitting them buys a cross-contract
authorization dance and a window in which each disagrees about whether a
proposal passed. Members with weights, quorum and approval thresholds in basis
points, a voting window, a timelock, a rolling-window auto-spend limit, and a
recipient allowlist.

`execute` deliberately takes no authorization: anyone may execute a proposal
that has passed and cleared its timelock. Restricting it to the bot would make
our uptime a treasury liveness dependency, which is the worst property a
treasury tool can have.

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
| 11 | MCP server, then MCP client |
| 12 | Mainnet rails; the marketing rebuild |

## Verification

- `make check` (fmt, vet, test) at every phase boundary; `make test-race` and
  `make fuzz-money` before a deploy. Tests bring their own Postgres, so none of
  this needs Docker or a local database.
- A cross-language golden corpus for transaction description: the same bytes
  asserted by a Go test and a Node test. Without it the two implementations
  drift and the guarantee disappears silently.
- `go test -tags=integration ./settlement/` against live testnet and friendbot,
  creating its own dependencies rather than relying on a third party's account.
- `cargo test` including negative-authorization cases run *without*
  `mock_all_auths` — a contract suite that only runs under mocked auth proves
  nothing about authorization.
- The end-to-end bar: a stranger adds the bot to their own Discord server and
  Telegram group, runs setup, links a wallet by signature, links a treasury,
  proposes a payment, approves it in the browser, and watches it land on
  testnet — recognised as the same member on both platforms.

## Not yet designed

- Confirming a Soroban contract call honestly in a browser that cannot, in
  general, re-derive what the call will do.
- Concurrent proposals against one treasury (CAP-21 preconditions).
- A remote signer for the operator key.
- Compliance: Travel Rule, sanctions screening, SEP-12 KYC.
- Self-hosted Horizon and Soroban RPC rather than the public instances.
