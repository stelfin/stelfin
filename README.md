# stelfin

**A non-custodial DAO treasury bot on Stellar, run from Telegram and Discord.**

stelfin lets a DAO act on-chain from the group chat it already lives in: payments,
payroll, trustlines, swaps and contract calls, each a proposal that goes to the
treasury's signers. `/pay 250 to G…` opens a proposal; a sentence like "send 5 to mum" is turned
into a structured instruction and checked against the sender's own words. Either
way a human confirms it in a browser that re-derives the transaction from the XDR
before signing it.

The bot never holds a member's key. It adapts to the signing authority the DAO
already runs — a single signer or an M-of-N classic multisig — building and routing
transactions while the network decides whether they are authorized. The operator's
own key sponsors reserves and pays fees, and can move nothing else.

Beyond treasury operations, stelfin is a place to connect the other tools a community
already uses: a spreadsheet, somebody's MCP server, an endpoint that returns rows. A
connector is used only once the treasury's signers have signed a statement of what it
may do, at the weight a payment takes. What it returns is text: a payroll read from a
sheet becomes a draft, a person proposes it, and the signers sign that too.

The seams, the data model and the decisions behind them — why non-custodial, why the
browser re-derives what it signs, what was accepted on purpose — are in
[docs/architecture.md](docs/architecture.md).

> **Status: testnet.** The path from a chat message to a multisig payment on chain
> works and is exercised end to end by `make e2e`, as are trustlines and swaps.
> What is not built is listed in [docs/architecture.md](docs/architecture.md):
> contract-held treasuries are next. Nothing here is on mainnet, and the server
> refuses to start there unless told to in so many words.

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
4. Confirming submits (`POST /v1/submit`), and the server settles the signed
   payment on Stellar (`settlement/`), paying its fee.
5. An ingestion worker (`ingestion/`) streams confirmed transactions back from
   Horizon and records them in an append-only, double-entry ledger (`ledger/`)
   backed by Postgres. The ledger is an index of what the chain did, never the
   authority for a balance.

A member with no wallet goes through `POST /v1/enroll` → `POST /v1/enroll/submit`,
which provisions a Stellar account for them (sponsored reserve, no custody) before
their first payment. Provisioning costs the operator real XLM, so it is off until
an admin of a workspace with a proved treasury turns it on, and capped after that;
`/reclaim` hands an account back and releases its reserve.

## Tech stack

| Layer           | Technology                                      |
| --------------- | ----------------------------------------------- |
| Language        | Go 1.25                                         |
| Blockchain      | `github.com/stellar/go-stellar-sdk`             |
| Database        | PostgreSQL via `pgx/v5`, migrations via `goose` |
| Messaging       | Telegram Bot API, Discord Interactions           |
| Intent decoding | Any OpenAI-compatible model, or Anthropic Claude; a fixed grammar with neither |

## Getting started

**Prerequisites:** Go 1.25+ and Node. No Postgres, no Docker — the tests and
the local server both bring up an embedded instance (see `internal/pgtest`).

```bash
git clone https://github.com/stelfin/stelfin.git
cd stelfin

# Format, vet, and run the full test suite
make check

# Run the server locally, on testnet. Reads .env if there is one, and
# generates whatever development keys it does not provide.
make dev

# Walk the whole flow against live testnet: set up a workspace, link a
# wallet, prove a 2-of-3 treasury, propose, sign, execute, check the chain.
make e2e

# The same, then every signing page walked in headless Chrome.
make e2e-browser
```

[docs/running-locally.md](docs/running-locally.md) covers connecting a real
Telegram bot and what each command does. A deployment runs `cmd/stelfind`
instead, configured entirely through its environment — see below.

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
| `STELFIN_WEBAUTH_SEED`           | With a chat platform | Signs SEP-10 challenges. Must differ from the treasury seed. Required once Telegram or Discord is configured: without it nobody can link a wallet or a treasury. |
| `STELFIN_TELEGRAM_BOT_TOKEN`     | No       | Telegram bot token. Required together with the webhook secret.                          |
| `STELFIN_TELEGRAM_WEBHOOK_SECRET`| No       | Echoed back on every delivery — Telegram does not sign, so this authenticates it. ≥32. stelfin registers the webhook itself at start. |
| `STELFIN_TELEGRAM_API_URL`       | No       | Bot API root, for a self-hosted Bot API server. https, or this machine.                 |
| `STELFIN_DISCORD_PUBLIC_KEY`     | No       | Application public key. Verifies the Ed25519 signature on each delivery.                |
| `STELFIN_DISCORD_BOT_TOKEN`      | No       | Discord bot token. Required with the public key and application id.                     |
| `STELFIN_DISCORD_APPLICATION_ID` | No       | Application snowflake, for followups and command registration.                          |
| `STELFIN_READONLY`               | No       | Kill switch: reads keep working, nothing is built or submitted.                         |
| `STELFIN_MAX_PAYMENT` / `_DAILY` | Mainnet  | The deployment's own ceilings on treasury proposals. Workspaces set lower with `/limits`. |
| `STELFIN_MAINNET_ACK`            | Mainnet  | Must say `this deployment moves real funds`, or the server will not start on `public`.  |
| `STELFIN_READONLY_FILE`          | No       | While a file exists at this path, nothing is built or submitted. No restart needed.      |
| `STELFIN_CONTRACT_CALLS`         | No       | Allows calling allowlisted contracts. Off: such calls are not bounded by the ceilings.   |
| `STELFIN_CONNECTOR_KEY`          | No       | Seals connector endpoints and tokens at rest. Enables MCP and webhook connectors.        |
| `STELFIN_GOOGLE_SERVICE_ACCOUNT` | No       | Google service-account JSON (or `_FILE`). Enables spreadsheet connectors.                |
| `STELFIN_CONFIRM_TOKEN_SECRET`   | Yes      | Signs confirmation links. `openssl rand -hex 32`.                                       |
| `STELFIN_DECODER_API_KEY` / `_MODEL` / `_BASE_URL` | No | A model from any provider speaking the OpenAI format (Groq, OpenRouter, OpenAI…) to read sentences. |
| `ANTHROPIC_API_KEY`              | No       | The same, from Anthropic. With no model at all, a sentence is read only as `send 5 to mum`. |

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
| `api/decoder`       | Free text → structured intent: a model, backed by a fixed grammar. |
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
| `internal/app`      | Wires everything above together and runs it.                |
| `cmd/stelfind`      | Server entrypoint, configured through the environment.      |
| `cmd/stelfin-dev`   | The same server for a laptop: embedded Postgres, `.env`.    |
| `cmd/stelfinctl`    | The operator's tool: list workspaces, suspend or resume one. |
| `connector/`        | The connector model and guard; Sheets, MCP client, webhook, egress. |
| `mcp/`              | stelfin as an MCP server at `POST /mcp`: read, and a tier that can only draft. |
| `e2e/`              | The whole flow against live testnet (`make e2e`).           |

## Documentation

| Document                                     | What it covers                                                                                                    |
| -------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| [docs/running-locally.md](docs/running-locally.md) | Running it on a laptop, connecting a Telegram bot, and the flow command by command. |
| [docs/architecture.md](docs/architecture.md) | The seams, the data model, the build sequence — and everything still left to do. |

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md)
before opening a pull request, and note that this project handles real money
movement — see the [Code Standards](CONTRIBUTING.md#code-standards) section for
what that means in practice (no shortcuts around verification, no untrusted
input treated as a signing decision).

All contributors are expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE)
