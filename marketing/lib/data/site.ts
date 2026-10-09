export const SITE = {
  name: "stelfin",
  legalName: "stelfin",
  githubUrl: "https://github.com/stelfin/stelfin",
};

// Where the bot is, once it is somewhere.
//
// The site used to have a chat button that pointed at wa.me/000000000000: a
// placeholder that read as a working front door and went nowhere. A dead call
// to action is worse than none, because it spends the one moment a visitor was
// willing to act. So nothing here links to a bot until it is told where one
// is — and it is told by the environment, at build time, and not by an edit:
//
//   NEXT_PUBLIC_STELFIN_TELEGRAM_BOT     the bot's username, with or without @
//   NEXT_PUBLIC_STELFIN_DISCORD_APP_ID   the Discord application's id
//
// With neither set the site is what it was: the button leads to the source,
// and the FAQ says nothing is live. With either set, the buttons, the
// "how do I get started" answer and the "Try it on testnet" section all change
// together, which is the point of doing it in one place — a live button above
// an answer saying nothing is live is worse than either alone.
//
// A value that is not the shape of a username or an id is treated as not set.
// The safe way to be wrong here is to stay closed.
const TELEGRAM_USERNAME = /^[A-Za-z][A-Za-z0-9_]{4,31}$/;
const DISCORD_SNOWFLAKE = /^\d{17,20}$/;

// Written out in full each time. The bundler replaces these exact expressions
// with their values at build time, and reads nothing it has to work out.
const telegramUsername = (process.env.NEXT_PUBLIC_STELFIN_TELEGRAM_BOT ?? "").trim().replace(/^@/, "");
const discordApplication = (process.env.NEXT_PUBLIC_STELFIN_DISCORD_APP_ID ?? "").trim();
const assetIssuer = (process.env.NEXT_PUBLIC_STELFIN_ASSET_ISSUER ?? "").trim();

export const BOT = {
  telegram: TELEGRAM_USERNAME.test(telegramUsername)
    ? {
        // A chat with the bot, which introduces itself and says what comes next.
        chat: `https://t.me/${telegramUsername}`,
        // Telegram's own "add this bot to a group" chooser.
        addToGroup: `https://t.me/${telegramUsername}?startgroup=true`,
      }
    : null,
  discord: DISCORD_SNOWFLAKE.test(discordApplication)
    ? {
        // The scope the bot needs and no more: its commands. It reads no
        // messages and holds no permission in the server.
        addToServer: `https://discord.com/oauth2/authorize?client_id=${discordApplication}&scope=applications.commands`,
      }
    : null,
};

/** Whether there is a bot to send anybody to. */
export const LIVE = BOT.telegram !== null || BOT.discord !== null;

// Circle's USDC issuer on Stellar testnet. The faucet step in "Try it on
// testnet" is shown only when this deployment's asset is that one: Circle's
// faucet hands out Circle's USDC, and sending somebody to it for a bot that
// calls some other asset "USDC" would be sending them for the wrong token.
const CIRCLE_TESTNET_USDC = "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5";
export const PAYS_IN_CIRCLE_USDC = assetIssuer === CIRCLE_TESTNET_USDC;

// `kind` exists so the icon travels with the destination: the icon used to be
// hardcoded inside the hero, which meant swapping the href left a GitHub mark
// sitting on a Telegram button.
export type CtaKind = "source" | "telegram" | "discord";

export interface Cta {
  kind: CtaKind;
  href: string;
  label: string;
}

const SOURCE_CTA: Cta = { kind: "source", href: SITE.githubUrl, label: "View the source" };
const TELEGRAM_CTA: Cta | null = BOT.telegram
  ? { kind: "telegram", href: BOT.telegram.chat, label: "Open in Telegram" }
  : null;
const DISCORD_CTA: Cta | null = BOT.discord
  ? { kind: "discord", href: BOT.discord.addToServer, label: "Add to Discord" }
  : null;

/** The one primary call to action: a bot if there is one, the source if not. */
export const PRIMARY_CTA: Cta = TELEGRAM_CTA ?? DISCORD_CTA ?? SOURCE_CTA;

/** The other platform, when the bot is on both. */
export const SECONDARY_CTA: Cta | null = TELEGRAM_CTA && DISCORD_CTA ? DISCORD_CTA : null;

export const NAV_LINKS = [
  { href: "#how-it-works", label: "How it works" },
  { href: "#security", label: "Security" },
  ...(LIVE ? [{ href: "#try-it", label: "Try it" }] : []),
  { href: "#faq", label: "FAQ" },
];
