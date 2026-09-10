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

## The call to action

There is deliberately no "Add to Telegram" button. The bot is not reachable
yet, and a dead call to action spends the one moment a visitor was willing to
act. Switching it on is two edits, and they must happen together:

1. `PRIMARY_CTA` in `lib/data/site.ts`
2. the `get-started` answer in `lib/data/faqs.ts`, which currently says nothing
   is live yet

Both carry comments saying so.
