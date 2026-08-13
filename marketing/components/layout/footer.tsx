"use client";

import { useRef } from "react";
import { motion, useScroll, useTransform } from "framer-motion";
import { BrandMark } from "@/components/ui/brand-mark";
import { SITE } from "@/lib/data/site";

export function Footer() {
  const root = useRef<HTMLElement | null>(null);
  // The wordmark rises slightly as the footer scrolls into view — slow and
  // small, so it registers as depth rather than as an animation. Ported
  // from the portfolio footer's GSAP scrub, in Framer Motion terms.
  const { scrollYProgress } = useScroll({ target: root, offset: ["start end", "end end"] });
  const y = useTransform(scrollYProgress, [0, 1], ["18%", "0%"]);

  return (
    <footer ref={root} className="relative isolate overflow-hidden border-t border-surface-50/10 bg-ink-900 pt-14 text-surface-50">
      <div className="relative z-10 mx-auto w-full max-w-[1400px] px-6">
        <div className="flex flex-col gap-10 md:flex-row md:items-start md:justify-between">
          <div>
            <BrandMark />
            <p className="mt-3 max-w-xs text-base text-surface-50/60">
              Non-custodial stablecoin payments, by message. Built on Stellar.
            </p>
            <p className="mt-2 text-sm text-surface-50/40">Testnet demo — not for real-money use yet.</p>
          </div>

          <nav aria-label="Footer" className="flex items-center gap-2">
            <a
              href={SITE.githubUrl}
              target="_blank"
              rel="noopener noreferrer"
              aria-label="Source on GitHub"
              className="flex h-10 w-10 items-center justify-center rounded-full border border-surface-50/15 text-surface-50/60 transition-colors duration-200 hover:border-accent-300/50 hover:text-accent-300"
            >
              <GithubGlyph />
            </a>
            <a
              href={SITE.appUrl}
              target="_blank"
              rel="noopener noreferrer"
              aria-label="Open the app"
              className="flex h-10 w-10 items-center justify-center rounded-full border border-surface-50/15 text-surface-50/60 transition-colors duration-200 hover:border-accent-300/50 hover:text-accent-300"
            >
              <ExternalGlyph />
            </a>
          </nav>
        </div>

        <div className="relative z-10 mt-10 border-t border-surface-50/10 py-6 text-sm text-surface-50/40">
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
            fontSize: "clamp(3rem, 14vw, 14rem)",
            letterSpacing: "0.01em",
            maskImage: "linear-gradient(to bottom, transparent, black 55%)",
            WebkitMaskImage: "linear-gradient(to bottom, transparent, black 55%)",
          }}
          className="block whitespace-nowrap text-center font-display font-bold leading-[0.78] text-surface-50/[0.06]"
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
function ExternalGlyph() {
  return (
    <svg viewBox="0 0 20 20" width={16} height={16} fill="none" aria-hidden="true">
      <path d="M8 5H5a1 1 0 0 0-1 1v9a1 1 0 0 0 1 1h9a1 1 0 0 0 1-1v-3" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M11 4h5v5M15.5 4.5 9 11" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
