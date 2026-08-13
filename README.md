# stelfin

**A non-custodial, WhatsApp-native stablecoin wallet on Stellar.**

stelfin lets someone send USDC over WhatsApp — no app install, no seed phrase shown
to the user, no custodian holding funds. A message like `send 20 usdc to +234...` is
decoded into a structured intent, verified against strict per-user rules, and
confirmed by the user on a signed link before anything settles on Stellar. The
operator never has signing authority over user funds: it can sponsor reserves and
pay fees, but every payment is authorized by the user's own device key.

This is the third iteration of an idea already tried twice, both times custodial and
both times outgrowing their stack. stelfin reverses the defaults: non-custodial,
Go-first, precision over shipping speed. The full reasoning — why custody was
rejected, why Go, why the intent-verification scheme looks the way it does, what
tradeoffs were accepted on purpose — is written up in
[DESIGN.md](DESIGN.md), and the request/settle flow in
[docs/architecture.md](docs/architecture.md).

---

## Table of contents

- [How it works](#how-it-works)
- [Tech stack](#tech-stack)
- [Getting started](#getting-started)
- [Environment variables](#environment-variables)
- [Project layout](#project-layout)
- [Documentation](#documentation)
- [Contributing](#contributing)
- [License](#license)

---

## How it works

1. A user messages the WhatsApp number. The webhook (`POST /webhook/whatsapp`)
   receives it and hands the text to an LLM-backed decoder (`api/decoder`) that
   extracts a structured, untrusted intent — never a signing decision by itself.
2. The intent is tokenized, normalized, and verified (`api/intent`) against
   per-user rules: known recipient formats, amount bounds, replay protection.
3. The user gets back a confirmation link (`GET /v1/confirm`) carrying a
   signed, single-use token. Opening it renders the confirmation page
   (`web/`) with the exact terms of the payment.
4. Confirming submits (`POST /v1/submit`) and the server builds and settles the
   payment on Stellar (`settlement/`), recorded first in an append-only,
   double-entry ledger (`ledger/`) backed by Postgres.
5. A separate ingestion worker (`ingestion/`) streams confirmed Stellar
   transactions back from Horizon to reconcile the ledger against the chain.

New users go through `POST /v1/enroll` → `POST /v1/enroll/submit`, which
provisions their Stellar account (sponsored reserve, no custody) before their
first payment.

## Tech stack

| Layer          | Technology                                  |
| -------------- | -------------------------------------------- |
| Language       | Go 1.25                                      |
| Blockchain     | `github.com/stellar/go-stellar-sdk`          |
| Database       | PostgreSQL via `pgx/v5`, migrations via `goose` |
| Messaging      | Meta WhatsApp Cloud API                      |
| Intent decoding| Anthropic Claude (`anthropic-sdk-go`)        |

## Getting started

**Prerequisites:** Go 1.25+. No local Postgres setup needed — tests bring up
their own embedded instance on first run (see `internal/pgtest`).

```bash
# Clone the repository
git clone https://github.com/ezedike-evan/stelfin.git
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

| Variable                       | Required | Description                                                        |
| ------------------------------- | -------- | -------------------------------------------------------------------- |
| `STELFIN_BASE_URL`              | Yes      | Where the confirmation page is served from. Must be https (or `http://localhost`). |
| `STELFIN_DATABASE_URL`          | Yes      | Postgres connection string.                                         |
| `STELFIN_NETWORK`               | No       | `testnet` (default) or `public`.                                    |
| `STELFIN_TREASURY_SEED`         | Yes      | Pays fees, sponsors reserves. Testnet-only as an env var — move to KMS/HSM for mainnet. |
| `STELFIN_ASSET_CODE` / `_ISSUER`| Yes      | The asset users transact in (network-specific issuer).              |
| `STELFIN_META_*`                | Yes      | Meta WhatsApp Cloud API credentials.                                 |
| `STELFIN_CONFIRM_TOKEN_SECRET`  | Yes      | Signs confirmation links. `openssl rand -hex 32`.                   |
| `ANTHROPIC_API_KEY`             | No       | Omit to let the Anthropic SDK resolve credentials itself.           |

## Project layout

| Path              | What it is                                                        |
| ----------------- | ------------------------------------------------------------------- |
| `api/`            | HTTP server, webhook handling, submission, orchestration.          |
| `api/intent`      | Tokenizer, verifier, normalizer, resolver for user intents.        |
| `api/decoder`     | Claude-backed free-text → structured-intent decoder.               |
| `internal/money`  | Exact fixed-point money type.                                      |
| `ledger/`         | Append-only, double-entry Postgres ledger and migrations.          |
| `settlement/`     | Stellar transaction building and submission.                       |
| `ingestion/`      | Horizon → ledger reconciliation worker.                            |
| `internal/whatsapp`| Meta WhatsApp Cloud API client.                                    |
| `internal/config` | Environment loading and validation.                                |
| `web/`            | Confirmation and enrollment pages served to the user.               |
| `cmd/stelfind`    | Server entrypoint — wires everything above together.               |

## Documentation

| Document                                   | What it covers                                                        |
| ------------------------------------------- | ------------------------------------------------------------------------ |
| [DESIGN.md](DESIGN.md)                     | Full design record: custody decision, auth model, intent-verification scheme, accepted tradeoffs, open questions. |
| [docs/architecture.md](docs/architecture.md)| Architecture plan and how this project relates to its two predecessors. |

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md)
before opening a pull request, and note that this project handles real money
movement — see the [Code Standards](CONTRIBUTING.md#code-standards) section for
what that means in practice (no shortcuts around verification, no untrusted
input treated as a signing decision).

All contributors are expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE)
