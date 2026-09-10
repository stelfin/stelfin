"use client";

import { motion } from "framer-motion";
import { stagger, fadeUp } from "@/lib/animation/variants";

// The three claims that used to sit inside the hero.
//
// They were evicted rather than deleted: a hero is one moment, and a headline
// plus a subhead plus two CTAs plus a three-item trust strip is a feature list
// wearing a hero's clothes. Here they get their own band, directly below, where
// they read as the answer to the question the headline just raised.
//
// Deliberately a band, not three cards. Three equal cards side by side is the
// single most templated layout on the web, and these are three short labels;
// wrapping each in a bordered box would add chrome and communicate nothing.
const CLAIMS = [
  { icon: <LockGlyph />, label: "Non-custodial", detail: "stelfin never holds a signing key" },
  { icon: <SponsorGlyph />, label: "Sponsored", detail: "signers need no XLM of their own" },
  { icon: <CheckGlyph />, label: "Verified in the browser", detail: "the page re-reads the transaction itself" },
];

export function TrustBand() {
  return (
    <section className="border-y border-line bg-bg-sunken">
      <motion.div
        initial="hidden"
        whileInView="visible"
        viewport={{ once: true, amount: 0.4 }}
        variants={stagger(0.08, 0)}
        className="mx-auto grid max-w-[1200px] gap-px bg-line sm:grid-cols-3"
      >
        {CLAIMS.map((claim) => (
          <motion.div
            key={claim.label}
            variants={fadeUp(0, 12)}
            className="flex flex-col gap-2 bg-bg-sunken px-6 py-8 sm:px-8 sm:py-10"
          >
            <span className="text-accent" aria-hidden="true">
              {claim.icon}
            </span>
            <span className="text-[15px] font-medium text-fg">{claim.label}</span>
            <span className="text-sm leading-relaxed text-fg-subtle">{claim.detail}</span>
          </motion.div>
        ))}
      </motion.div>

      <p className="mx-auto max-w-[1200px] px-6 py-4 text-center text-sm text-fg-subtle sm:px-8">
        Running on Stellar testnet. Not for real money yet.
      </p>
    </section>
  );
}

function LockGlyph() {
  return (
    <svg viewBox="0 0 20 20" width={20} height={20} fill="none" aria-hidden="true">
      <rect x="4" y="8.5" width="12" height="8" rx="1.5" stroke="currentColor" strokeWidth="1.5" />
      <path d="M6.5 8.5V6a3.5 3.5 0 0 1 7 0v2.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  );
}

function SponsorGlyph() {
  return (
    <svg viewBox="0 0 20 20" width={20} height={20} fill="none" aria-hidden="true">
      <circle cx="10" cy="10" r="7" stroke="currentColor" strokeWidth="1.5" />
      <path
        d="M10 6.5v7M7.5 8.3c0-1 .9-1.8 2.5-1.8s2.5.7 2.5 1.6c0 2.1-5 1.3-5 3.4 0 .9 1 1.6 2.5 1.6s2.5-.6 2.5-1.6"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
      />
    </svg>
  );
}

function CheckGlyph() {
  return (
    <svg viewBox="0 0 20 20" width={20} height={20} fill="none" aria-hidden="true">
      <path d="m5 10.5 3.2 3.2L15 7" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
