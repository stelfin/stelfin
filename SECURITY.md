# Security policy

stelfin builds and routes transactions that move real money, and it asks members to
sign them. A flaw in how it does that matters more than a flaw in most projects, so
please report one privately.

## Reporting a vulnerability

**Do not open a public issue, pull request or discussion for a vulnerability.**

Use GitHub's private reporting instead: on the repository page, open the
**Security** tab and choose **Report a vulnerability**. That reaches the
maintainers alone.

Please include:

- what you found and which part of stelfin it affects (a package, a contract, a
  signing page, a deployment setting);
- the steps or a test that reproduces it, against testnet where it needs a network;
- what an attacker gains: whose money or authority, and under what conditions.

We will acknowledge a report within a few days, tell you whether we agree it is a
vulnerability, and keep you informed until it is fixed. We will credit you in the
fix unless you would rather we did not.

## What is in scope

The properties [docs/architecture.md](docs/architecture.md) says stelfin holds. A
break in any of these is a vulnerability:

- **Custody.** The bot never holds a member's key. Anything that lets the server,
  the operator key, or a chat message sign for a member is in scope.
- **What is signed is what was shown.** The browser re-derives the transaction from
  the XDR before the sign button appears. Any way to make a page show one thing
  and sign another is in scope.
- **Untrusted input stays untrusted.** Decoder (LLM) output, connector output and
  chat text must pass `api/intent` verification and the member's own confirmation
  before anything settles. Any path around that is in scope.
- **Authentication of deliveries.** Telegram secret-token and Discord Ed25519
  checks, and the signed, single-use confirmation, enrolment and link tokens.
- **The contracts** in `contracts/`: any way to move a treasury's funds without the
  vote and wait its rules require, or to take authority the contract does not give
  its relay.
- **The ledger.** A way to make the double-entry books disagree with the chain, or
  to post the same event twice.

## What is out of scope

- Decisions the architecture document records as made on purpose, and limits it
  lists under "What is left". Report these only if you can show the stated
  reasoning does not hold.
- Findings that need a stolen operator secret, a compromised member device, or
  control of the member's wallet. These are assumed lost already.
- Testnet being reset, rate-limited, or short of funds.
- Denial of service by volume against a free-tier deployment.
- Reports from automated scanners with no demonstrated impact.

## Supported versions

stelfin runs on **testnet** and refuses to start on mainnet unless told to in so
many words. Fixes go to `main`; there are no release branches to patch.

## Testing it yourself

Run your own instance (see [docs/running-locally.md](docs/running-locally.md)) and
test against testnet with accounts you created. Please do not test against anyone
else's deployment, workspace, or treasury, and do not read or alter data that is
not yours.
