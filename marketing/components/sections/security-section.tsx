"use client";

import { useRef } from "react";
import { motion, useScroll, useTransform } from "framer-motion";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";

const PILLARS = [
  {
    id: "key",
    title: "Your key never leaves your device",
    body: "stelfin cannot move your funds. Every send is signed by you — the server never holds a spending key, so there's nothing here to steal or subpoena.",
    illustration: <KeyLockAnimation />,
  },
  {
    id: "confirm",
    title: "Confirmed on a page that reads the transaction itself",
    body: "The confirmation screen parses the envelope you're about to sign, not our word for it. If our summary ever disagreed with the transaction, it would refuse to show a send button at all.",
    illustration: <ConfirmGlyph />,
  },
  {
    id: "model",
    title: "The model never decides an amount or address",
    body: "An LLM helps read your message, but every amount and destination it claims is checked against your own words before anything is built to sign. It extracts; it never authorizes.",
    illustration: <ModelGlyph />,
  },
];

export function SecuritySection() {
  const ref = useRef<HTMLElement | null>(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start end", "end start"] });
  const backgroundColor = useTransform(scrollYProgress, [0, 0.15, 0.85, 1], ["#eaf7ef", "#0e120f", "#0e120f", "#ffffff"]);

  return (
    <motion.section ref={ref} id="security" style={{ backgroundColor }} className="relative overflow-hidden py-16 text-surface-50 md:py-28">
      <div className="relative mx-auto max-w-7xl px-6 sm:px-[72px]">
        <div className="mx-auto max-w-[820px] text-center">
          <Reveal>
            <div className="font-mono text-xs uppercase tracking-[0.2em] text-surface-50/40">Security</div>
          </Reveal>
          <MaskReveal
            as="h2"
            text="Built non-custodial, on purpose."
            accent="on purpose."
            className="mt-8 justify-center font-display text-[34px] font-normal leading-[1.02] tracking-[-0.03em] md:text-[56px] lg:text-[64px]"
          />
          <Reveal delay={0.15}>
            <p className="mx-auto mt-6 max-w-xl text-base leading-relaxed text-surface-50/60 md:text-xl">
              Software you hold the key to — not a business holding your funds.
            </p>
          </Reveal>
        </div>

        <div className="mt-16 grid gap-6 md:mt-24 md:grid-cols-3 md:gap-8">
          {PILLARS.map((p, i) => (
            <Reveal key={p.id} delay={i * 0.15}>
              <article className="flex h-full flex-col gap-6 rounded-3xl border border-surface-50/10 bg-surface-50/[0.02] p-8 transition-colors duration-300 hover:border-surface-50/20 hover:bg-surface-50/[0.04] md:p-10">
                <div className="flex h-16 w-16 items-center justify-center rounded-2xl bg-surface-50/[0.04] text-accent-300">
                  {p.illustration}
                </div>
                <div>
                  <h3 className="font-sans text-2xl font-medium tracking-tight text-surface-50">{p.title}</h3>
                  <p className="mt-3 text-base leading-relaxed text-surface-50/60 md:text-lg">{p.body}</p>
                </div>
              </article>
            </Reveal>
          ))}
        </div>
      </div>
    </motion.section>
  );
}

// The key slides down and turns into the lock, on a loop — a small,
// literal illustration of the claim in the copy beside it, rather than a
// static padlock glyph.
function KeyLockAnimation() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7 overflow-visible" aria-hidden="true">
      <g className="animate-lock-pulse" style={{ transformOrigin: "16px 21px" }}>
        <rect x="7" y="15" width="18" height="13" rx="3" stroke="currentColor" strokeWidth="1.6" fill="none" />
        <circle cx="16" cy="20.5" r="1.6" fill="currentColor" />
        <path d="M16 22v2.5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      </g>
      <g className="animate-key-slide">
        <circle cx="16" cy="7" r="3.2" stroke="currentColor" strokeWidth="1.6" fill="none" />
        <path d="M16 10.2V16M16.2 13h2M16.2 14.6h1.4" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      </g>
    </svg>
  );
}
function ConfirmGlyph() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden="true">
      <rect x="7" y="5" width="18" height="24" rx="2.5" stroke="currentColor" strokeWidth="1.5" fill="none" />
      <path d="m11.5 17 4 4 7-9" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" fill="none" />
    </svg>
  );
}
function ModelGlyph() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden="true">
      <rect x="6" y="10" width="20" height="14" rx="4" stroke="currentColor" strokeWidth="1.5" fill="none" />
      <circle cx="12.5" cy="17" r="1.6" fill="currentColor" />
      <circle cx="19.5" cy="17" r="1.6" fill="currentColor" />
      <path d="M16 10V6m-3 0h6" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  );
}
