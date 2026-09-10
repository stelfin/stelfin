"use client";

import { useEffect, useRef, useState } from "react";
import { motion, useScroll, useTransform } from "framer-motion";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";

const PILLARS = [
  {
    id: "key",
    title: "A signer's key never leaves their device",
    body: "stelfin cannot move the treasury. Every approval is signed by the person giving it, and the server holds no spending key, so there is nothing here to steal or subpoena.",
    illustration: <KeyLockAnimation />,
  },
  {
    id: "confirm",
    title: "Approved on a page that reads the transaction itself",
    body: "The approval screen parses the envelope you are about to sign rather than trusting our summary of it. If the two ever disagreed, it would refuse to show a sign button at all.",
    illustration: <ConfirmGlyph />,
  },
  {
    id: "model",
    title: "The model never decides an amount or an address",
    body: "An LLM helps read the message, but every amount and destination it claims is checked against the sender's own words before anything is built to sign. It extracts; it never authorizes.",
    illustration: <ModelGlyph />,
  },
];

// The page's one permitted theme inversion.
//
// Framer Motion interpolates colours numerically and cannot read a var(), so
// the stops have to be resolved values. Reading them from the document at mount
// is what lets the inversion follow the theme: in light mode the section drops
// to the dark slab, and in dark mode, where the page is already dark, it goes a
// step further down to the sunken tone instead. Same beat, opposite direction.
// The literals below are the light-mode values, used for the server render.
function useInversionStops() {
  const [stops, setStops] = useState(["#fafaf9", "#0e120f", "#0e120f", "#fafaf9"]);

  useEffect(() => {
    const read = () => {
      const css = getComputedStyle(document.documentElement);
      const value = (name: string, fallback: string) => css.getPropertyValue(name).trim() || fallback;
      const page = value("--bg", "#fafaf9");
      const slab = value("--surface-inverse", "#0e120f");
      setStops([page, slab, slab, page]);
    };
    read();

    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener("change", read);
    return () => mq.removeEventListener("change", read);
  }, []);

  return stops;
}

export function SecuritySection() {
  const ref = useRef<HTMLElement | null>(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start end", "end start"] });
  const stops = useInversionStops();
  const backgroundColor = useTransform(scrollYProgress, [0, 0.15, 0.85, 1], stops);

  return (
    <motion.section
      ref={ref}
      id="security"
      style={{ backgroundColor }}
      className="on-inverse relative overflow-hidden py-16 text-on-surface-inverse md:py-28"
    >
      <div className="relative mx-auto max-w-[1100px] px-6 sm:px-[72px]">
        <div className="max-w-[780px]">
          <Reveal>
            <div className="font-mono text-xs uppercase tracking-[0.2em] text-on-surface-inverse/40">Security</div>
          </Reveal>
          <MaskReveal
            as="h2"
            text="Built non-custodial, on purpose."
            accent="on purpose."
            className="mt-8 text-[34px] font-medium leading-[1.02] tracking-[-0.03em] md:text-[56px] lg:text-[64px]"
          />
          <Reveal delay={0.15}>
            <p className="mt-6 max-w-xl text-base leading-relaxed text-on-surface-inverse/60 md:text-xl">
              Software the DAO holds the keys to, not a business holding its funds.
            </p>
          </Reveal>
        </div>

        {/* A stacked list separated by hairlines, not three equal cards.
            Three identical bordered boxes side by side is the most templated
            layout on the web, and it also forced these three quite different
            claims into one shape. As rows they can breathe, the illustration
            sits beside its own sentence, and the reading order is unambiguous
            on every width. */}
        <div className="mt-16 md:mt-24">
          {PILLARS.map((p, i) => (
            <Reveal key={p.id} delay={i * 0.12}>
              <article className="grid gap-5 border-t border-on-surface-inverse/10 py-10 md:grid-cols-[auto_1fr] md:gap-10 md:py-14">
                <div className="flex h-14 w-14 items-center justify-center rounded-surface bg-on-surface-inverse/[0.05] text-accent">
                  {p.illustration}
                </div>
                <div className="md:max-w-[46rem]">
                  <h3 className="text-2xl font-medium tracking-tight text-on-surface-inverse md:text-[28px]">
                    {p.title}
                  </h3>
                  <p className="mt-3 text-base leading-relaxed text-on-surface-inverse/60 md:text-lg">{p.body}</p>
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
// The checkmark draws itself onto the document, on a loop — "verified"
// happening repeatedly, rather than a static check glyph.
function ConfirmGlyph() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden="true">
      <rect x="7" y="5" width="18" height="24" rx="2.5" stroke="currentColor" strokeWidth="1.5" fill="none" />
      <path
        d="m11.5 17 4 4 7-9"
        pathLength={1}
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
        fill="none"
        className="animate-check-draw"
      />
    </svg>
  );
}
// A scan line sweeps the model's face, top to bottom, on a loop — reading
// continuously, deciding nothing, matching "it extracts; it never
// authorizes" in the copy beside it.
function ModelGlyph() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden="true">
      <defs>
        <clipPath id="model-face-clip">
          <rect x="6" y="10" width="20" height="14" rx="4" />
        </clipPath>
      </defs>
      <rect x="6" y="10" width="20" height="14" rx="4" stroke="currentColor" strokeWidth="1.5" fill="none" />
      <circle cx="12.5" cy="17" r="1.6" fill="currentColor" />
      <circle cx="19.5" cy="17" r="1.6" fill="currentColor" />
      <path d="M16 10V6m-3 0h6" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      <g clipPath="url(#model-face-clip)">
        <rect x="6" y="10" width="20" height="3" fill="currentColor" className="animate-scan-sweep" />
      </g>
    </svg>
  );
}
