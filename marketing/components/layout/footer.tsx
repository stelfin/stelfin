"use client";

import { useRef } from "react";
import { motion, useScroll, useTransform } from "framer-motion";
import { BrandMark } from "@/components/ui/brand-mark";
import { SITE } from "@/lib/data/site";

export function Footer() {
  const root = useRef<HTMLElement | null>(null);
  // The wordmark rises slightly as the footer scrolls into view — slow and
  // small, so it registers as depth rather than as an animation.
  const { scrollYProgress } = useScroll({ target: root, offset: ["start end", "end end"] });
  const y = useTransform(scrollYProgress, [0, 1], ["18%", "0%"]);

  return (
    <footer
      ref={root}
      className="on-inverse relative isolate overflow-hidden border-t border-line bg-surface-inverse pt-14 text-on-surface-inverse"
    >
      <div className="relative z-10 mx-auto w-full max-w-[1400px] px-6">
        <div className="flex flex-col gap-10 md:flex-row md:items-start md:justify-between">
          <div>
            <BrandMark />
            <p className="mt-3 max-w-xs text-base text-on-surface-inverse/60">
              Non-custodial DAO treasury operations on Stellar, run from the group chat.
            </p>
            <p className="mt-2 text-sm text-on-surface-inverse/60">
              Testnet demo. Not for real-money use yet.
            </p>
          </div>

          {/* One destination, not two.
              The second icon linked to SITE.appUrl (the Render service), whose
              "/" redirects straight back to this marketing site — so the only
              other call to action in the footer returned the visitor to the
              page they were already on. That is the dead-CTA failure this
              file's sibling comment in lib/data/site.ts exists to prevent.
              It comes back when there is somewhere real to send people. */}
          <nav aria-label="Footer" className="flex items-center gap-2">
            <a
              href={SITE.githubUrl}
              target="_blank"
              rel="noopener noreferrer"
              aria-label="Source on GitHub"
              className="flex h-10 w-10 items-center justify-center rounded-full border border-on-surface-inverse/15 text-on-surface-inverse/60 transition-colors duration-200 hover:border-accent/50 hover:text-accent"
            >
              <GithubGlyph />
            </a>
          </nav>
        </div>

        <div className="relative z-10 mt-10 border-t border-on-surface-inverse/10 py-6 text-sm text-on-surface-inverse/60">
          © {new Date().getFullYear()} {SITE.legalName}. All rights reserved.
        </div>
      </div>

      {/* The wordmark: sized in vw so it spans edge to edge at any width,
          cropped by the footer's bottom edge, faded upward with a mask so it
          reads as printed texture rather than as a line of copy. Hidden from
          the accessibility tree — the name is already announced above. */}
      <div aria-hidden="true" className="pointer-events-none relative -z-0 mt-4 select-none overflow-hidden">
        <motion.span
          style={{
            y,
            fontSize: "clamp(2.5rem, 9vw, 9rem)",
            // Spread as wide as the wordmark can go while staying one line —
            // it's meant to read as texture spanning the footer, not as a
            // tightly-set logotype.
            letterSpacing: "0.6em",
            maskImage: "linear-gradient(to bottom, transparent, black 55%)",
            WebkitMaskImage: "linear-gradient(to bottom, transparent, black 55%)",
          }}
          className="block whitespace-nowrap text-center font-bold leading-[0.78] text-on-surface-inverse/[0.06]"
        >
          stelfin
        </motion.span>
      </div>
    </footer>
  );
}

function GithubGlyph() {
  return (
    <svg viewBox="0 0 24 24" width={17} height={17} fill="currentColor" aria-hidden="true">
      <path d="M12 2C6.48 2 2 6.58 2 12.19c0 4.49 2.87 8.3 6.84 9.65.5.1.68-.22.68-.49 0-.24-.01-1.03-.01-1.87-2.78.61-3.37-1.19-3.37-1.19-.46-1.17-1.11-1.48-1.11-1.48-.91-.63.07-.62.07-.62 1 .07 1.53 1.04 1.53 1.04.89 1.54 2.34 1.1 2.91.84.09-.65.35-1.1.63-1.35-2.22-.26-4.56-1.13-4.56-5.01 0-1.11.39-2.01 1.03-2.72-.1-.26-.45-1.3.1-2.71 0 0 .84-.27 2.75 1.04a9.4 9.4 0 0 1 5 0c1.91-1.31 2.75-1.04 2.75-1.04.55 1.41.2 2.45.1 2.71.64.71 1.03 1.61 1.03 2.72 0 3.89-2.34 4.75-4.57 5 .36.32.68.94.68 1.9 0 1.37-.01 2.47-.01 2.81 0 .27.18.6.69.49A10.02 10.02 0 0 0 22 12.19C22 6.58 17.52 2 12 2Z" />
    </svg>
  );
}
