# Running stelfin locally

Everything here is testnet. Nothing needs installing but Go and Node.

## Start it

```bash
make dev
```

That runs `cmd/stelfin-dev`, which is the real server (`internal/app`) with the
three things a laptop lacks supplied for it:

- **A database.** An embedded Postgres on port 54329, with its data under
  `.dev/postgres`, so a restart resumes where it stopped. Set
  `STELFIN_DATABASE_URL` yourself and it uses yours instead.
- **The environment.** `.env` is read, and anything already set in your shell
  wins over it. The server proper never reads `.env`.
- **Throwaway keys.** Any of the confirm secret, the operator seed and the
  web-auth seed that `.env` does not provide is generated once and kept under
  `.dev/`. An operator account Horizon has never seen is funded by friendbot.

It refuses to run against the public network. Delete `.dev/` to start over.

With no chat platform configured the server still comes up and serves the
signing pages; it says at startup that nothing can reach the bot.

## Prove the whole flow works

```bash
make e2e
```

This is the end-to-end bar from [architecture.md](architecture.md), with the
chat platform replaced by a local stand-in for the Telegram Bot API and
everything else real: the `stelfind` binary as a separate process, a real
Postgres, and Stellar testnet through the public Horizon and friendbot. It takes
two or three minutes and needs network access.

It sets a workspace up, links a wallet by SEP-10 signature, proves a 2-of-3
treasury with two signatures, proposes a payment, is refused execution at one
signature, grants a second member a role, collects the second signature,
executes, and checks the balances on chain. Then the other path: a member with
no wallet writes "send 3 to G…", is told provisioning is off, an admin turns it
on, she is refused for being too new, is set up a wallet that holds no XLM,
pays from it twice with the operator carrying the fee, and hands it back.
After that: a workspace limit refuses a proposal, a payment to an account that
does not exist is refused before anyone signs, a member pays from a wallet she
linked and signs in, an agent reads over MCP with a token minted in chat and is
shut out when it is revoked; a spreadsheet connector is refused until two
signers sign its grant and stopped the moment it is revoked, and a webhook is
refused an address on this machine or inside the network — and then a Discord
server is connected to the same
workspace, where the same person is recognised as the same member, grants a
role from Discord's member picker, and signs the proposal opened on Telegram.
Last, the asset's own contract is deployed for the run's USDC, allowed, and
paid through: a contract call from the 2-of-3 treasury, refused at one
signature and executed at two.

The Go test plays the browser by calling the JSON endpoints the pages call.
To have the pages themselves walked, in headless Chrome:

```bash
make e2e-browser        # CHROME=chromium to pick a different binary
```

It opens every link the bot handed out and presses what a person would press,
on all five pages: enroll, approve, link (a wallet, then a treasury), confirm
and reclaim. To walk them yourself instead:

```bash
E2E_HOLD=10m make e2e
```

That leaves the server up and writes links, and the signers' throwaway testnet
seeds, to `e2e/e2e-hold.txt` — more are appended as each becomes possible.

## Connectors

A spreadsheet needs `STELFIN_GOOGLE_SERVICE_ACCOUNT` (the key JSON from a Google
Cloud service account with the Sheets API enabled). A workspace shares its sheet
with that account's address, as a viewer, and `/connector add` says which address.

An MCP server or a webhook needs `STELFIN_CONNECTOR_KEY`, which seals its address
and token at rest. Both are entered on a page, never in chat, and the address has
to be https on the public internet: stelfin resolves it once, refuses anything
private, and connects to the address it checked.

Nothing is called until the treasury's signers have signed the grant. A payroll
sheet is read with headings `address` and `amount` (and optionally `memo`), or
by position in that order when it has no headings.

## Operating it

```bash
STELFIN_DATABASE_URL=… go run ./cmd/stelfinctl orgs
STELFIN_DATABASE_URL=… go run ./cmd/stelfinctl suspend 7     # and: resume 7
```

Or, without a restart: set `STELFIN_READONLY_FILE=/run/stelfin/readonly`, and
`touch` that file to stop everything, `rm` it to resume.

A suspended workspace keeps its history readable and moves nothing through this
deployment. It takes effect on the next message. To stop everything at once, set
`STELFIN_READONLY=true` in the server's environment and restart it.

## Connect a real Telegram bot

A webhook needs a public https URL, so this needs a tunnel to your machine.

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
   In BotFather, turn **group privacy off** for it, or it only ever sees
   commands addressed to it by name.
2. Start a tunnel to port 8080 and note its https URL.
3. In `.env`:

   ```bash
   STELFIN_BASE_URL=https://<your-tunnel>
   STELFIN_TELEGRAM_BOT_TOKEN=<token>
   STELFIN_TELEGRAM_WEBHOOK_SECRET=<openssl rand -hex 32>
   ```

4. `make dev`, then point Telegram at it, passing the same secret:

   ```bash
   curl "https://api.telegram.org/bot<token>/setWebhook" \
     -d url="https://<your-tunnel>/webhook/telegram" \
     -d secret_token="<the secret>"
   ```

5. Add the bot to a group. **Every member who will use it must open a private
   chat with the bot and press Start once.** Telegram has no private replies in
   a group, so stelfin answers in a direct message, and a bot cannot message
   someone who has never started a chat with it.

## Connect a real Discord application

1. Create an application in the Discord developer portal and add a bot to it.
2. In `.env`: `STELFIN_DISCORD_PUBLIC_KEY`, `STELFIN_DISCORD_BOT_TOKEN` and
   `STELFIN_DISCORD_APPLICATION_ID`, all three from the portal.
3. `make dev`, then set the application's **Interactions Endpoint URL** to
   `https://<your-tunnel>/webhook/discord`. Discord checks it on save by
   sending a signed ping, so the server has to be running.
4. Invite the application to a server with the `applications.commands` scope.

Discord delivers a bot nothing but slash commands, so a sentence is sent with
`/send`. Every reply is visible only to whoever asked.

`STELFIN_BASE_URL` is the origin every signing link is minted under. If it
points anywhere other than where this server is reachable, the links in the
bot's replies open somebody else's page.

## The flow, in chat

| Who | Types | What happens |
|---|---|---|
| a group admin | `/setup Acme DAO` | The group becomes a workspace and they become its admin. |
| anyone | `/whoami` | Their member number, role and linked wallet. |
| anyone | `/link G…` | A link to a page where they sign a challenge proving the address is theirs. |
| an admin | `/grant 12 approver` | Member #12 gets a role. Roles: observer, proposer, approver, admin. On Discord, pick the person instead. |
| an admin | `/link_treasury G…` | A link to a page where the account's signers prove control, with the weight a payment needs. |
| a proposer | `/pay 250 to G… march invoice` | A proposal, and their own link to sign it. |
| any signer | `/approve` | Their own link to sign the open proposal. |
| anyone with a role | `/execute` | Submits it, once it has the signatures. |
| an admin | `/provisioning on` | Members with no wallet can be set one up. Needs a linked treasury. Off by default. |
| any member | `send 5 to G…` | From the wallet stelfin set up for them: a link to confirm exactly that payment. |
| its owner | `/reclaim` | A link to hand a provisioned wallet back, once it is empty. |
| anyone | `/save mum G…`, `/saved`, `/forget mum` | Names for addresses, private to whoever saved them: "send 5 to mum". |
| anyone | `/send 5 to mum` | The same as writing the sentence. On Discord, the only way to. |
| an admin | `/limits payment 5000 daily 20000` | Ceilings on what is put to this treasury's signers. `/limits` shows them. |
| an admin | `/connect` | A code that adds another server or group to this workspace. |
| a space admin | `/connect <code>` | In the new space: joins it to the workspace the code came from. |
| a member with two accounts | `/link_code`, then `/claim <code>` | From the account with a wallet, then the other: one member, both platforms. |
| an admin | `/agent-token <label>` | A read-only token for an outside agent, over MCP at `POST /mcp`. Shown once. |
| an admin | `/agent-tokens`, `/agent-revoke <n>` | List them, and withdraw one. |
| an admin | `/connector add payroll sheets` | Registers a connector. Grants it nothing. Kinds: sheets, mcp, webhook. |
| an admin | `/connector setup rates` | A page where an MCP server's or webhook's address and token are entered — never in chat. |
| an admin | `/connector grant payroll` | A page where the treasury's signers sign a statement of what it may do. |
| an admin | `/connector revoke payroll` | Stops it at once. Takes no signatures, on purpose. |
| anyone with a role | `/connectors`, `/read payroll <sheet id>!A1:C20` | List them; show what one returns. |
| a proposer | `/payroll payroll <sheet id>!A2:C40 memo:March` | Reads a payroll into a draft. Proposes nothing. |
| a proposer | `/drafts`, `/propose 3`, `/discard 3` | Look at drafts; put one to the signers as it was drafted; drop one. |
| an admin | `/contract allow C… usdc`, `/contract remove usdc` | Allow a contract, pinned to the code it runs now. Authorises no call. |
| a proposer | `/invoke usdc transfer address:G… address:G… i128:50000000` | Propose a call from the treasury. Arguments are typed. |
| a proposer | `/restore usdc transfer …` | When a call needs archived state, propose the restore on its own. |
| an admin | `/agent-token payroll-bot propose` | A token that may also write drafts, marked as the agent's, for a member to propose. |
| an admin | `/grant approver` as a reply | On Telegram, grant the person whose message you reply to. |

A role decides what the bot will offer someone. It never decides whether a
signature counts — that is the treasury account's own signer list, enforced by
the network.

On Telegram a command cannot contain a hyphen, so the menu offers
`/link_treasury` and `/link_code`. Typing them with a hyphen works too.

Writing in your own words needs `ANTHROPIC_API_KEY`: a model finds the amount
and the recipient in the sentence, and what it finds is checked against the
words before anything is built. The slash commands need no model at all.

The treasury pays its own transaction fee, so it needs a little XLM, and the
destination needs a trustline to the asset in `STELFIN_ASSET_CODE` /
`STELFIN_ASSET_ISSUER` or the network refuses the payment.
