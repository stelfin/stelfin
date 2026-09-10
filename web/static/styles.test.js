// Every page carries the rules that keep it usable and readable.
//
// These five pages have no shared stylesheet. The content security policy in
// web/embed.go allows inline style and nothing else, so each page carries its
// own near-copy of the same block, and near-copies drift. They had: confirm and
// enroll, the two pages most people actually reach, were the only two with no
// focus ring and no hover state at all, and no page had ink chosen for the
// dark-mode accent, so every primary button rendered white-on-#4ec27f at
// 2.25:1.
//
// Nobody noticed because nothing was looking. A shared file would have made the
// drift impossible, but it would also mean loosening style-src for a stylesheet
// a person opens twice a year. This test buys the same guarantee for nothing:
// fix an accessibility problem on three pages and forget the other two, and the
// suite says so.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

// Each rule is a name and something that must appear in every page's markup.
// Deliberately substring checks rather than a CSS parse: the point is to catch
// a page that was skipped, not to police how a rule is written.
const REQUIRED = [
  {
    name: "hides [hidden] against any display rule",
    needle: "[hidden] { display: none !important; }",
    why: "the scripts show and hide every section by toggling .hidden, and a page whose flex or grid rule outranks it would reveal a section that is meant to be closed",
  },
  {
    name: "defines ink for the accent ground",
    needle: "--on-accent:",
    why: "white on the dark-mode accent #4ec27f is 2.25:1, which fails WCAG AA outright",
  },
  {
    name: "gives keyboard users a visible focus ring",
    needle: ":focus-visible",
    why: "these pages authorise payments, and a keyboard user who cannot see what is focused cannot see what they are about to activate",
  },
  {
    name: "gives pointer users a hover state",
    needle: "button:hover:not([disabled])",
    why: "a button with no hover feedback reads as inert",
  },
  {
    name: "honours prefers-reduced-motion",
    needle: "@media (prefers-reduced-motion: reduce)",
    why: "the button transform is small, but a stated preference is not a suggestion",
  },
  {
    name: "stacks key/value rows on a narrow screen",
    needle: "@media (max-width: 26rem)",
    why: "a 56-character Stellar address in a space-between row has nowhere to go at 400px, and the address is the thing being checked",
  },
  {
    name: "supports dark mode",
    needle: "@media (prefers-color-scheme: dark)",
    why: "a signer on a dark system should not be flashed a white page at the moment they are reading an amount",
  },
  {
    name: "declares the shared type scale",
    needle: "--t-mono:",
    why: "the sizes drifted to nine values, five of which sat inside a 2px band and read as one size; the tokens are what keep the steps distinct",
  },
  {
    name: "treats .note as left-aligned by default",
    needle: ".note.center {",
    why: ".note meant centred on three pages and left on two, with opposite opt-in classes, so the same class name did two different things depending on which page you were reading",
  },
  {
    name: "makes the alert box focusable",
    needle: 'id="alert" class="alert" tabindex="-1"',
    why: "a refusal moves focus to this box so a screen reader announces it; without a tabindex the focus call is silently a no-op",
  },
];

const pages = fs
  .readdirSync(__dirname)
  .filter((f) => f.endsWith(".html"))
  .sort();

test("every page carries the rules that keep it usable", () => {
  assert.ok(pages.length > 0, "no pages found");

  const missing = [];
  for (const page of pages) {
    const html = fs.readFileSync(path.join(__dirname, page), "utf8");
    for (const rule of REQUIRED) {
      if (!html.includes(rule.needle)) {
        missing.push(`${page} ${rule.name}: ${rule.why}`);
      }
    }
  }

  assert.deepStrictEqual(missing, [], "\n  " + missing.join("\n  ") + "\n");
});

// The rules above are only worth anything if they are checked against every
// page. A page added to this directory and forgotten is the same failure this
// file exists to prevent, one level up.
test("every page this directory serves is checked", () => {
  const served = ["approve.html", "confirm.html", "enroll.html", "link.html", "reclaim.html"];
  assert.deepStrictEqual(
    pages,
    served,
    "a page was added or removed; add it to this list once you have confirmed it carries the rules above",
  );
});
