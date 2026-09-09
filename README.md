# stelfin

**A non-custodial DAO treasury bot on Stellar, run from Telegram and Discord.**

stelfin lets a DAO act on-chain from the group chat it already lives in: payments,
payroll, trustlines, swaps, contract calls and proposals. A message like
`/pay 5000 USDC to ada` is turned into a structured instruction, checked against the
user's own words, and confirmed by a human in a browser that re-derives the
transaction from the XDR before signing it.

The bot never holds a member's key. It adapts to whatever signing authority the DAO
already runs — a single signer, an M-of-N classic multisig, or a Soroban smart
account — building and routing transactions while the network decides whether they
are authorized. The operator's own key sponsors reserves and pays fees, and can move
nothing else.

Beyond treasury operations, stelfin is a place to connect the other tools a community
touches on-chain: spreadsheets, models, trading venues, and any MCP server a DAO
registers. A connector can propose; only a human can approve.

The full reasoning — why non-custodial, why Go, why the intent-verification scheme
looks the way it does, what tradeoffs were accepted on purpose — is in
[DESIGN.md](DESIGN.md), and the build plan in
[docs/architecture.md](docs/architecture.md).

> **Status: rebuild in progress.** stelfin began as a WhatsApp-native personal wallet.
> That transport is gone and the DAO surfaces are being built in phases — see the
> build sequence in [docs/architecture.md](docs/architecture.md). Nothing here is on
> mainnet, and no chat transport is registered yet.

---

## Table of contents

- [How it works](#how-it-works)
- [Tech stack](#tech-stack)
- [Getting started](#getting-started)
- [Environment variables](#environment-variables)
- [Contracts](#contracts)
- [Project layout](#project-layout)
- [Documentation](#documentation)
- [Contributing](#contributing)
- [License](#license)

---

## How it works

1. A member messages the bot. The webhook (`POST /webhook/{channel}`) authenticates
   the delivery, parses it into a channel-agnostic message, and either dispatches a
   slash command or hands free text to an LLM-backed decoder (`api/decoder`) that
   extracts a structured, untrusted intent — never a signing decision by itself.
2. The intent is tokenized, normalized, and verified (`api/intent`) against
   per-user rules: known recipient formats, amount bounds, replay protection.
3. The member gets back a private confirmation link (`GET /v1/confirm`) carrying a
   signed, single-use token in the URL fragment. Opening it renders the confirmation
   page (`web/`), which parses the envelope itself and refuses to show a sign button
   if its reading disagrees with the server's summary.
4. Confirming submits (`POST /v1/submit`) and the server builds and settles the
   payment on Stellar (`settlement/`), recorded first in an append-only,
   double-entry ledger (`ledger/`) backed by Postgres.
5. A separate ingestion worker (`ingestion/`) streams confirmed Stellar
   transactions back from Horizon to reconcile the ledger against the chain.

A member with no wallet goes through `POST /v1/enroll` → `POST /v1/enroll/submit`,
which provisions a Stellar account for them (sponsored reserve, no custody) before
their first payment. Provisioning costs the operator real XLM, so it is rate-limited.

## Tech stack

| Layer           | Technology                                      |
| --------------- | ----------------------------------------------- |
| Language        | Go 1.25                                         |
| Blockchain      | `github.com/stellar/go-stellar-sdk`             |
| Database        | PostgreSQL via `pgx/v5`, migrations via `goose` |
| Messaging       | Telegram Bot API, Discord Interactions           |
| Intent decoding | Anthropic Claude (`anthropic-sdk-go`)           |

## Getting started

**Prerequisites:** Go 1.25+. No local Postgres setup needed — tests bring up
their own embedded instance on first run (see `internal/pgtest`).

```bash
# Clone the repository
git clone https://github.com/stelfin/stelfin.git
cd stelfin

# Copy the example environment file and fill in your values
cp .env.example .env

# Format, vet, and run the full test suite
make check

# Run the server directly
go run ./cmd/stelfind
```

Other useful targets: `make test-race`, `make cover`, `make fuzz-money`,
`make tidy`. Run `make help` for the full list.

## Environment variables

Copy `.env.example` to `.env` and fill in every required value — the server
validates configuration at boot (`internal/config`) and refuses to start rather
than run with a placeholder secret. See `.env.example` for the full list with
descriptions; the essentials:

| Variable                         | Required | Description                                                                             |
| -------------------------------- | -------- | --------------------------------------------------------------------------------------- |
| `STELFIN_BASE_URL`               | Yes      | Where the confirmation page is served from. Must be https (or `http://localhost`).      |
| `STELFIN_DATABASE_URL`           | Yes      | Postgres connection string.                                                             |
| `STELFIN_NETWORK`                | No       | `testnet` (default) or `public`.                                                        |
| `STELFIN_TREASURY_SEED`          | Yes      | Pays fees, sponsors reserves. Testnet-only as an env var — move to KMS/HSM for mainnet. |
| `STELFIN_ASSET_CODE` / `_ISSUER` | Yes      | The asset users transact in (network-specific issuer).                                  |
| `STELFIN_WEBAUTH_SEED`           | No       | Signs SEP-10 challenges. Must differ from the treasury seed. Without it, no linking.    |
| `STELFIN_TELEGRAM_BOT_TOKEN`     | No       | Telegram bot token. Required together with the webhook secret.                          |
| `STELFIN_TELEGRAM_WEBHOOK_SECRET`| No       | Echoed back on every delivery — Telegram does not sign, so this authenticates it. ≥32.  |
| `STELFIN_DISCORD_PUBLIC_KEY`     | No       | Application public key. Verifies the Ed25519 signature on each delivery.                |
| `STELFIN_DISCORD_BOT_TOKEN`      | No       | Discord bot token. Required with the public key and application id.                     |
| `STELFIN_DISCORD_APPLICATION_ID` | No       | Application snowflake, for followups and command registration.                          |
| `STELFIN_CONFIRM_TOKEN_SECRET`   | Yes      | Signs confirmation links. `openssl rand -hex 32`.                                       |
| `ANTHROPIC_API_KEY`              | No       | Omit to let the Anthropic SDK resolve credentials itself.                               |

## Contracts

Two Soroban contracts, in `contracts/`, written in Rust.

**`dao_treasury`** holds a DAO's policy and its proposals in one state machine:
members with voting weight, quorum and approval measured separately, a voting
period, a timelock, a rolling auto-spend allowance that starts at zero, and a
recipient allowlist that starts empty. `execute` takes no authorisation — once a
proposal has passed and its timelock has run, anyone may carry it out, because a
contract whose funds need a specific person online is a contract with a hostage.

**`connector_registry`** records what a DAO has allowed each connector to do.
It stores no credential, no token and no endpoint URL: everything in a contract
is public, so the endpoint is committed to as a salted hash. It pins the
connector's declared tool surface, which is the check a capability list cannot
make — a server granted "read a spreadsheet" that later answers with a tool
transferring funds passes every capability check and fails this one.

### What is on chain

Uploaded to **testnet**. One WASM upload per network; a contract instance per
DAO is created on demand and recorded in Postgres, not here.

| Contract             | WASM hash                                                          | Upload                                                                                                              |
| -------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------- |
| `dao_treasury`       | `3e9ffbfbd7f3c71d0821f8f15c0a1b1206ba4ae738eeb82800292b9598a93c78` | [tx](https://stellar.expert/explorer/testnet/tx/9b59999c5131807db77944c8a8baf12d9199d6268dd768acf908947978ab0b70) |
| `connector_registry` | `903898770dba6b113fb7f60c3f81cc430d3492613ba876993829be952b10d25e` | [tx](https://stellar.expert/explorer/testnet/tx/66eaf4fd5ea8e721ed734373b39a174fad96db8854df9d3963ecae905835d501) |

Those hashes are not taken on trust. `contracts/deployments.json` is embedded in
the binary, one test rebuilds both contracts and checks the hashes match this
source, and `go test -tags=integration ./contracts/` reads the contract-code
entry back off the network and hashes what it returns. The last of those cannot
pass unless the upload really happened.

```bash
make test-contracts    # the Rust suites, including the negative-auth tests
make build-contracts   # the release WASM the committed hashes come from
make test-deploy       # prove those hashes are on testnet (needs network)
```

Mainnet is deliberately absent. A contract exists on testnet long before it
exists on mainnet, and `contracts.Lookup` reports that as an ordinary state
rather than an error.

## Project layout

| Path                | What it is                                                  |
| ------------------- | ----------------------------------------------------------- |
| `api/`              | HTTP server, webhook handling, submission, orchestration.   |
| `api/intent`        | Tokenizer, verifier, normalizer, resolver for user intents. |
| `api/decoder`       | Claude-backed free-text → structured-intent decoder.        |
| `internal/money`    | Exact fixed-point money type.                               |
| `ledger/`           | Append-only, double-entry Postgres ledger and migrations.   |
| `settlement/`       | Stellar transaction building and submission.                |
| `ingestion/`        | Horizon → ledger reconciliation worker.                     |
| `chat/`             | The platform-agnostic contract every transport implements.  |
| `core/`             | Routing: tenancy, the delivery claim, roles, commands.      |
| `identity/`         | SEP-10 challenges, for proving control of an address.       |
| `ledger/store`      | Every query in the system, org-scoped.                      |
| `internal/telegram` | The Telegram Bot API transport.                             |
| `internal/discord`  | The Discord interactions transport.                         |
| `internal/config`   | Environment loading and validation.                         |
| `contracts/`        | The Soroban contracts, in Rust, and what is on chain.       |
| `signer/`           | The seam between stelfin and anything holding a key.        |
| `web/`              | Confirmation and enrollment pages served to the user.       |
| `marketing/`        | The public site, a separate Next.js module.                 |
| `cmd/stelfind`      | Server entrypoint — wires everything above together.        |

## Documentation

| Document                                     | What it covers                                                                                                    |
| -------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| [DESIGN.md](DESIGN.md)                       | Full design record: custody decision, auth model, intent-verification scheme, accepted tradeoffs, open questions. |
| [docs/architecture.md](docs/architecture.md) | Architecture plan and how this project relates to its two predecessors.                                           |

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md)
before opening a pull request, and note that this project handles real money
movement — see the [Code Standards](CONTRIBUTING.md#code-standards) section for
what that means in practice (no shortcuts around verification, no untrusted
input treated as a signing decision).

All contributors are expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE)
