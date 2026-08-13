# Contributing to stelfin

Thanks for considering a contribution. stelfin moves real money, so the bar for
changes touching intent verification, settlement, or the ledger is higher than
for a typical web app — read this before opening a pull request.

## Before you begin

- Read [DESIGN.md](DESIGN.md) first. It explains *why* the system is shaped the
  way it is: why custody was rejected, why the intent-verification scheme has
  the checks it has, and what tradeoffs were accepted on purpose rather than by
  accident. A change that looks like a simplification is often reopening a
  question already settled there.
- Read the [Code of Conduct](CODE_OF_CONDUCT.md). All participation is expected
  to follow it.
- Open an issue before starting non-trivial work, so the approach can be agreed
  on before code is written.

## Development setup

```bash
git clone https://github.com/ezedike-evan/stelfin.git
cd stelfin
cp .env.example .env   # fill in real values, or use test defaults where noted
make check              # fmt + vet + full test suite
```

Tests bring up their own embedded Postgres — see `internal/pgtest` — so no
local database or Docker setup is required. The first run downloads a Postgres
binary and caches it under `$HOME`.

## Workflow

1. Fork the repo and create a branch off `main`.
2. Make your change, with tests. See [Code Standards](#code-standards) below
   for what's expected of tests in this repo specifically.
3. Run `make check` locally before opening a PR — CI will run the same thing.
4. Open a PR against `main` with a clear description of what changed and why.
   Link the issue it addresses.
5. Address review feedback. Once approved, a maintainer will merge.

## Code Standards

### Money

All amounts flow through `internal/money`, the fixed-point type built to avoid
float rounding on real currency. Never introduce a second numeric
representation of an amount (`float64`, raw `int64` cents, `string` parsed ad
hoc) anywhere on the path from a user's message to a settled payment. If
`internal/money` doesn't do something you need, extend it — don't work around
it.

### Trust boundaries

The decoder (`api/decoder`) turns free-text WhatsApp messages into a structured
intent using an LLM. That output is **untrusted** the same way any external
input is: it must pass through `api/intent`'s tokenizer, verifier, and
normalizer before it can affect a balance, and the user must confirm the exact
terms on the signed confirmation link before anything settles. Do not add a
path where decoder output — or any other unverified input — can trigger
settlement directly.

### Tests

- New logic in `internal/money`, `api/intent`, `ledger`, or `settlement` needs
  tests that exercise the edge cases, not just the happy path — these are the
  packages closest to moving money.
- `ledger` and `settlement` tests exercise Postgres/Stellar interaction
  directly rather than mocking it out; follow that pattern for new tests in
  those packages rather than introducing a mock-based style.
- Run `make test-race` before submitting anything touching concurrency
  (webhook handling, ingestion).

### Commits

This repo uses [Conventional Commits](https://www.conventionalcommits.org/)
with a scope, matching the existing history:

```
feat(api): add enroll tokens, versioned separately from confirm tokens
fix(ledger): correct rounding in double-entry balance check
```

Common types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`. Common scopes
match the top-level package touched: `api`, `ledger`, `settlement`,
`ingestion`, `web`, `config`.

## Pull request checklist

- [ ] `make check` passes locally.
- [ ] New/changed behavior has test coverage.
- [ ] If the change touches `internal/config` or `.env.example`, both are
      updated together.
- [ ] If the change touches the intent-verification scheme or custody model,
      [DESIGN.md](DESIGN.md) is updated to match.
- [ ] Commit messages follow the Conventional Commits format above.

## Questions

Open an issue, or start a discussion, if anything here is unclear or you're
unsure whether an approach fits before investing time in it.
