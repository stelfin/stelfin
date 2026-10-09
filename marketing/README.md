# stelfin marketing site

The public landing page at [stelfin.vercel.app](https://stelfin.vercel.app). A
separate Next.js app from the Go service in the repository root, deployed
separately, sharing nothing at runtime.

The one thing the two surfaces do share is the brand green, `#14713d`. It is
declared here in `app/globals.css` and again in every page under
`web/static/*.html`, which the Go binary serves. Changing it in one place and
not the other splits the product into two brands that happen to have the same
name.

## Running it

```bash
npm install
npm run dev        # http://localhost:3000
```

Checks, all three of which CI runs:

```bash
npx tsc --noEmit
npm run lint
npm run build
```

Note that `make check` at the repository root does **not** cover this
directory. `marketing/go.mod` exists solely to wall the app off from
`go build ./...` so that `node_modules` is never walked, and the side effect is
that the Go gate says nothing about the site. The `marketing` job in
`.github/workflows/ci.yml` is what actually gates it.

## Layout

```
app/
  layout.tsx           fonts, metadata, the motion and smooth-scroll providers
  page.tsx             the single route; composes the sections in order
  globals.css          the whole design system, in two layers (see below)
  icon.svg             the brand mark, source of favicon.ico and apple-icon.png
  opengraph-image.tsx  the share card, generated at build time
components/
  sections/            one file per section of the page
  layout/              navbar, footer
  ui/                  brand mark, CTA button, CTA icon, the world map
  interactive/         mask reveal, scroll reveal, smooth scroll
lib/
  data/                copy that more than one component needs
  animation/           shared motion variants and easing
```

## The design system

`app/globals.css` has two layers, and the distinction matters:

**Layer 1, `@theme`** is the raw palette: `surface-*`, `ink-*`, `accent-*`.
These are pigments. They do not change between light and dark.

**Layer 2** is the semantic tokens: `bg`, `fg`, `line`, `accent`, `on-accent`
and friends. They are named for the job rather than the colour, they flip under
`prefers-color-scheme: dark`, and they are what components should use. A
component written against `ink-900` is only correct in one of the two modes.

Two exceptions are deliberate:

- `components/ui/brand-mark.tsx` uses `accent-500` directly. A logo holds its
  brand colour in every context; that is what makes it a logo.
- Sections that invert (the Security band, the footer) carry the `.on-inverse`
  class, which redeclares the accent trio for their subtree. Their ground is
  dark whichever mode the page is in, so the page-level accent would be wrong
  inside them.

There is no `tailwind.config` file. Tailwind v4 is configured from CSS, and
`@theme inline` is what makes the semantic tokens resolve at use-time rather
than being frozen at build time.

## Motion

`framer-motion` for reveals and scroll-linked values, `lenis` for smooth
scrolling. No GSAP: mixing the two in one tree makes them fight over frames.

Everything honours `prefers-reduced-motion` through `MotionConfig
reducedMotion="user"` in the layout plus the global block in `globals.css`. The
one thing neither reaches is the travelling dots on the world map, which are
SMIL `<animateMotion>` and have no CSS hook at all; they carry an `.arc-dots`
class that the reduced-motion block hides outright.

## The call to action, and going live

The site does not link to a bot until it is told where one is, and it is told
by the environment at build time, not by an edit:

| Variable | What it is |
|---|---|
| `NEXT_PUBLIC_STELFIN_TELEGRAM_BOT` | The bot's Telegram username, with or without `@`. |
| `NEXT_PUBLIC_STELFIN_DISCORD_APP_ID` | The Discord application's id. |
| `NEXT_PUBLIC_STELFIN_ASSET_ISSUER` | The issuer the server is configured with (`STELFIN_ASSET_ISSUER`). |

With neither of the first two set, the site is closed: every button leads to
the source, and the last FAQ answer says nothing is live. Set either, in the
Vercel project's settings, and redeploy, and three things change together —
the buttons become the bot's (`lib/data/site.ts`), the "how do I get started"
answer becomes instructions (`lib/data/faqs.ts`), and the "Try it on testnet"
section appears with a link in the nav. They change together on purpose: a live
button above an answer saying nothing is live is worse than either alone.

A value that is not the shape of a username or an application id is treated as
not set. The safe way to be wrong here is to stay closed.

The third variable decides one step. "Get test USDC" sends a visitor to
Circle's faucet, which hands out Circle's testnet USDC; it is shown only when
the issuer given is Circle's, because for a server configured with any other
issuer that would be the wrong token.

Set them only once the bot answers. The server and this site deploy
separately, and the site should not say "open in Telegram" on a day the bot is
not there to be opened.
