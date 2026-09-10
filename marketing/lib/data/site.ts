// There is deliberately no chat CTA here yet.
//
// The previous one pointed at wa.me/000000000000 — a placeholder that read as a
// working front door and went nowhere. A dead call to action is worse than no
// call to action: it spends the one moment a visitor was willing to act. Until
// the Telegram and Discord bots exist, the honest destination is the source.
export const SITE = {
  name: "stelfin",
  legalName: "stelfin",
  githubUrl: "https://github.com/stelfin/stelfin",
};

// The single primary call to action, in one place, shaped so that switching it
// on is an edit rather than a hunt.
//
// `kind` exists so the icon travels with the destination: the icon used to be
// hardcoded inside the hero, which meant swapping the href left a GitHub mark
// sitting on a Telegram button. When the bot is live this becomes:
//
//   { kind: "telegram", href: "https://t.me/<bot>", label: "Open in Telegram" }
//
// and the only other edit is the `get-started` answer in lib/data/faqs.ts,
// which currently says nothing is live yet. Those two are the whole go-live
// change. Keep them together in that order so neither is forgotten.
export type CtaKind = "source" | "telegram";

export const PRIMARY_CTA: { kind: CtaKind; href: string; label: string } = {
  kind: "source",
  href: "https://github.com/stelfin/stelfin",
  label: "View the source",
};

export const NAV_LINKS = [
  { href: "#how-it-works", label: "How it works" },
  { href: "#security", label: "Security" },
  { href: "#faq", label: "FAQ" },
];
