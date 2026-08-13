# stelfin — architecture plan

## Context

stelfin is a WhatsApp-native stablecoin wallet on Stellar.

## Settled decisions

| Decision | Choice | Rationale |
|---|---|---|
| Chain | Stellar | native multisig, claimable balances, SEP standards, ~5s finality, no reorgs |
| Server language | Go | `stellar/go` is first-party; Go is the payments-infra norm |
| Contracts / custody / circuits | Rust | Soroban is Rust; one memory-safe language for all key-adjacent code |
| Custody model | Non-custodial | removes money-transmitter posture |
| Primary auth | zkLogin (Google OAuth, Groth16 on Soroban) | near-universal on target-market Android; passkeys are not |
| Second factor | PIN + device-stored 256-bit secret | no extra party, nothing public to grind offline |
| Wallet type | Soroban custom account (`C...`) | zkLogin verification must be on-chain to be trustless |
| Receiving address | Stable per user | accepted privacy tradeoff — see below |
| Ledger role | Index of on-chain state | not authoritative for balances |
| Web | TypeScript + Next.js | browser surfaces only |
| WhatsApp platform risk | Accepted | keep transport behind an interface regardless |

### Accepted tradeoff: address privacy

A stable `C...` per user means anyone able to send to a phone number can dust-probe that
number, learn the address, and read the user's entire transaction history on-chain,
permanently. This is a property of the send-to-phone-number feature, not a patchable bug.

Accepted deliberately. Two mitigations that cost nothing architecturally:
- The `phone → address` resolver is authenticated and rate-limited, so mass probing is
  expensive and attributable.
- Nothing identifying ever goes in a transaction memo — no phone numbers, no names, no
  guessable hashes (a phone-number hash is ~10^10 candidates, trivially reversed).

Revisit if the user base makes this a safety issue rather than a privacy one.

## Auth model

zkLogin is a **spend** path, so a compromised Google account is an immediate-loss vector
with no timelock behind it. The PIN carries that weight, and must never be attackable
offline. `salt = KDF(PIN)` with an on-chain commitment is rejected — a 6-digit PIN is
10^6 candidates against public chain state.

```
spend:
  zkLogin proof + H(PIN, device_secret)          → instant, to policy limit
  ... + policy co-sign (weight 1)                → above limit
  passkey where hardware supports it             → upgrade, replaces PIN

enroll  (have Google + PIN, no device secret):
  → 24h timelock, notified to existing factors, cancellable

recover (lost Google OR lost PIN):
  → any 2 of { zkLogin, SEP-30 phone, guardians }
  → proposes signer rotation only, never a spend
  → 7-day timelock, cancellable by any live spend factor
  → 30-day escalated override for stolen-device (thief holds the veto)
```

`device_secret` is 256-bit, generated client-side, never transmitted. Browser storage is
**not durable** — Safari ITP evicts after ~7 days idle, Android Chrome evicts under
pressure, "clear site data" wipes it. Storage eviction must therefore route to *enroll*
(24h), not *recover* (7d), or the product is unusable. Where WebAuthn PRF is available,
persist the secret there instead; it survives eviction.

Phone is never sufficient alone — SIM swap is common in the target market and carriers
recycle dormant numbers.

## Intent verification

The LLM is a decoder, never an authority. Every field it emits carries a provenance span
into the backend's own tokenization of the raw message.

```
backend tokenizes message      → indexed tokens, turn-indexed across turns
model returns fields + spans   → action / amount / destination, each with a span
backend re-verifies            → text == tokens[span]; reject on mismatch
backend normalizes             → "5,000" | "5k" | "five thousand" → int64 stroops
backend resolves               → beneficiary label → address, deterministic lookup
user signs normalized payload  → confirmation renders FROM the signed payload
```

Invariants, enforced by tests:
- The model never performs arithmetic and never emits a final amount.
- The model never resolves a recipient to an address.
- The **backend** tokenizes. A model-supplied text field is never trusted over `tokens[span]`.
- Ambiguous or unresolvable → ask, never guess.
- The confirmation UI derives its display from the XDR being signed, not from app state.
- Voice notes ground against a transcript that is itself model output — lower trust tier,
  tighter limits.

This defeats hallucinated amounts, invented recipients, and injection originating outside
the current message. Same-message injection still passes the span check by design — the
confirmation screen is what catches that.

## Dormancy close

Solves Soroban state archival: TTL maintenance becomes bounded by the dormancy window
instead of perpetual.

The sweep must be **pre-authorized by the user at onboarding** — destination fixed by
them, encoded in the contract or a pre-signed time-bounded transaction. stelfin may only
*trigger* an instruction the user already signed. Discretionary sweep authority is
custody regardless of the time delay, and would reintroduce exactly the licensing
exposure the non-custodial design buys away.

Needs a terminal fallback for a failed off-ramp destination (anchor down, KYC lapsed,
bank account closed) that does not resolve to "stelfin holds it". Dormancy window must be
long and heavily notified, over a channel the user may have lost.

## Target architecture

```
stelfin/
  contracts/          Rust — Soroban
    account/          custom account: signer registry, __check_auth, policies
    zk-verifier/      Groth16 over bls12_381 host fns
    jwks-registry/    Google signing keys, multi-updater + timelock
  circuits/           Rust/Circom — RS256 JWT + SHA-256, Groth16
  prover/             Rust — proof generation (not trust-sensitive)
  policy/             Rust — weight-limited co-signer: limits, velocity, risk
  recovery/           Go — SEP-30 recoverysigner, one of two independent servers
  settlement/         Go — Horizon + Soroban RPC, sponsorship, fee-bump, channel accounts
  ledger/             Go — double-entry index, reconciliation
  api/                Go — WhatsApp webhook, intent verification, HTTP API
  inbox/              Go — G... sweep account for classic + anchor interop
  web/                TypeScript + Next.js — confirm/sign, dashboard, recovery
```

Stack: Go 1.23+, pgx + sqlc (no ORM), goose migrations, River (Postgres-backed queue, so
enqueue joins the ledger transaction atomically). `stellar/go` for `horizonclient`,
`txnbuild`, `keypair`, `ingest`.

## Reuse from own services

| Source | What | Mode |
|---|---|---|
| `veil/contracts/invisible_wallet` | signer registry, session keys, spend limits, `initiate/complete/cancel_recovery` timelock+veto, nonce replay | **fork as Rust code** — the only real code reuse |
| `quay/packages/offramp` | `sep10/sep12/sep6/sep24/sep38` | network call to Quay API, or port to Go |
| `meridian` vault + BlendAdapter | yield on savings | on-chain, post-audit, later |
| `orbital/pulse-core` | Horizon cursor / backoff / rate-limit semantics | read and port to `stellar/go/ingest`; no code transfer |

Rule: own services may sit in the money-**adjacent** path (off-ramp, yield, analytics),
never the money-**critical** path. Send, receive, balance and custody must work with every
other stelfin service down.

## Milestone 1 — end-to-end thin slice

One user, one send, every seam connected. Proves integration before the hard parts land.

**Real in this slice** (not stubbed — without these there is no working Stellar wallet):
- CAP-33 sponsored account provisioning: `BeginSponsoringFutureReserves` → `CreateAccount`
  → `ChangeTrust` (USDC) → `EndSponsoringFutureReserves`, one atomic transaction. User
  holds zero XLM.
- CAP-15 fee-bump from a treasury account. User never pays fees.
- Double-entry ledger with `sum(entries) == 0` enforced in-database.
- Horizon ingestion with cursor persistence, confirming the ledger against chain state.

**Stubbed in this slice:**
- Soroban custom account → classic Stellar multisig, device-held Ed25519 key in browser.
- zkLogin, PIN, device secret, recovery, policy co-signer, off-ramp, yield.
- Channel-account pool (no contention with one user; note it and move on).

**Path to build:**
```
1. ledger/      schema + invariant tests, before anything touches a network
2. settlement/  sponsored provisioning + fee-bump, testnet
3. api/         WhatsApp webhook → deterministic tokenizer
4. api/         LLM decoder returning fields + spans; backend re-verifies spans
5. api/         normalization to int64 stroops; beneficiary resolution
6. web/         confirm page rendering from the payload; browser key signs
7. settlement/  submit, then Horizon ingestion writes the confirming ledger entry
```

LLM decoder: default to a current Claude model; confirm the exact model ID and pricing
against the `claude-api` reference at implementation time rather than from memory.

### Parallel spike — Groth16 resource budget

Runs alongside Milestone 1, not blocking it. This is the one deferred item that can
invalidate the architecture, and it is a measurement rather than a build.

```
1. RS256 JWT + SHA-256 circuit — count constraints
2. Groth16 verifier in Soroban using bls12_381 host functions
3. MEASURE CPU instructions + footprint for one __check_auth
4. compare against Soroban's per-transaction resource limits
```

Kill criterion: if verification does not fit the budget, zkLogin-on-chain is not viable
and the auth model needs rethinking before more is built on top of it. Fallbacks to
evaluate at that point: proof verification split across two transactions, a different
proving system, or off-chain verification behind an attested signer (weaker, trusted).

Verify current Soroban protocol capabilities against live docs — `secp256r1_verify` and
the `bls12_381` host functions were added in recent protocol versions and the exact
version numbers here are from memory, not checked.

## Verification for Milestone 1

```
go test ./...                    unit + property tests
go test -tags=integration ./...  against Stellar testnet
```

End-to-end, manually:
1. Send a WhatsApp message from a fresh number → user row + sponsored account with a USDC
   trustline exists on testnet, user holds 0 XLM.
2. Fund it from friendbot/testnet faucet → ingestion writes a matching credit entry.
3. Message "send 5,000 to <beneficiary>" → inspect the decoder output: every field has a
   span, and each span re-verifies against the backend's own tokens.
4. Confirm page shows the exact normalized amount and resolved address, both derived from
   the XDR under signature.
5. Sign → transaction lands on testnet, fee paid by treasury via fee-bump.
6. Ledger balances: `sum(entries) == 0` holds, and the indexed balance matches Horizon.

Adversarial cases that must fail closed:
- Decoder returns an amount whose span text doesn't match `tokens[span]` → rejected.
- Message containing an injected instruction → spans still ground to real text, and the
  confirmation shows the injected recipient rather than the intended one.
- Duplicate submission with the same idempotency key → one on-chain transaction, one
  ledger entry.
- Submit-then-timeout → resolution by deterministic transaction hash lookup, never by
  blind retry.

## Not yet designed

Each needs its own pass; listed so none is lost.

- `C...` vs `G...` interop — contract addresses cannot receive classic `Payment` ops. The
  inbox sweep account has a custodial window for in-flight deposits. Genuinely unsolved.
- Trusted setup ceremony — must be real, multi-party, publicly verifiable.
- JWKS oracle trust model — a malicious key updater can forge proofs for any account.
- Treasury key management — HSM/KMS, threshold signing, ceremony, rotation, break-glass.
- Channel-account pool for sequence-number contention.
- Deterministic simulation testing, `__check_auth` fuzzing, formal verification of the
  recovery state machine.
- Compliance — Travel Rule, sanctions screening, SEP-12 KYC tiers, NDPA 2023.
- Fraud — authorized-push-payment scams are unrefundable on-chain; reimbursement policy
  must be decided before launch.
- Nigerian Pidgin / Yoruba / Hausa / Igbo intent evals.
- Self-hosted Horizon + Soroban RPC rather than SDF public instances.
