"use client";

import { motion, useScroll, useTransform } from "framer-motion";
import { useRef } from "react";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";

const STEPS = [
  { n: "1", title: "Message stelfin", body: '"send 5,000 to my brother" — plain language.', icon: <MessageGlyph /> },
  { n: "2", title: "Confirm on your device", body: "The exact amount and recipient, nothing hidden.", icon: <DeviceGlyph /> },
  { n: "3", title: "It lands on Stellar", body: "Settled on-chain in seconds, sponsored.", icon: <ChainGlyph /> },
];

export function HowItWorksSection() {
  const sectionRef = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress: bgProgress } = useScroll({ target: sectionRef, offset: ["start end", "start start"] });
  const backgroundColor = useTransform(bgProgress, [0, 1], ["#fafaf9", "#eaf7ef"]);

  const lineRef = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress: lineProgress } = useScroll({ target: lineRef, offset: ["start 65%", "end 65%"] });
  const lineScale = useTransform(lineProgress, [0, 1], [0, 1]);

  return (
    <motion.section id="how-it-works" ref={sectionRef} style={{ backgroundColor }} className="relative py-20 md:py-28">
      <div className="mx-auto max-w-[1000px] px-6">
        <div className="mx-auto max-w-[560px] text-center">
          <Reveal>
            <span className="font-mono text-xs uppercase tracking-[0.2em] text-ink-400">How it works</span>
          </Reveal>
          <MaskReveal
            as="h2"
            text="Three steps, no forms"
            accent="no forms"
            className="mt-4 justify-center text-center font-display text-[32px] font-medium leading-[1.04] tracking-[-0.02em] text-ink-900 md:text-[48px]"
          />
        </div>

        <div ref={lineRef} className="relative mt-20 md:mt-28">
          {/* The connecting line: a static track plus a scroll-filled overlay
              that draws left to right (desktop) as the section scrolls by. */}
          <div className="absolute left-[10%] right-[10%] top-8 hidden h-[2px] -translate-y-1/2 bg-ink-200 md:block" />
          <motion.div
            style={{ scaleX: lineScale }}
            className="absolute left-[10%] right-[10%] top-8 hidden h-[2px] origin-left -translate-y-1/2 bg-accent-500 md:block"
          />
          {/* Mobile: the same idea, vertical. */}
          <div className="absolute bottom-0 left-8 top-0 w-[2px] bg-ink-200 md:hidden" />
          <motion.div style={{ scaleY: lineScale }} className="absolute bottom-0 left-8 top-0 w-[2px] origin-top bg-accent-500 md:hidden" />

          <div className="relative grid gap-14 md:grid-cols-3 md:gap-8">
            {STEPS.map((step, i) => (
              <Reveal key={step.n} delay={i * 0.15}>
                <div className="flex items-start gap-5 md:flex-col md:items-center md:text-center">
                  <div className="relative z-10 flex h-16 w-16 flex-none items-center justify-center rounded-full border-2 border-accent-500 bg-white text-accent-600 shadow-soft">
                    {step.icon}
                  </div>
                  <div className="md:mt-6">
                    <h3 className="text-lg font-medium tracking-tight text-ink-900">
                      <span className="mr-1.5 text-accent-500">{step.n}.</span>
                      {step.title}
                    </h3>
                    <p className="mt-1.5 max-w-[220px] text-[15px] leading-relaxed text-ink-500 md:mx-auto">{step.body}</p>
                  </div>
                </div>
              </Reveal>
            ))}
          </div>
        </div>
      </div>
    </motion.section>
  );
}

function MessageGlyph() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden="true">
      <path d="M6 8h20a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H14l-6 5v-5H6a2 2 0 0 1-2-2V10a2 2 0 0 1 2-2Z" stroke="currentColor" strokeWidth="1.6" fill="none" strokeLinejoin="round" />
    </svg>
  );
}
function DeviceGlyph() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden="true">
      <rect x="9" y="4" width="14" height="24" rx="2.5" stroke="currentColor" strokeWidth="1.6" fill="none" />
      <path d="m12.5 16 2.5 2.5 5-5.5" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" fill="none" />
    </svg>
  );
}
function ChainGlyph() {
  return (
    <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden="true">
      <rect x="6" y="13" width="11" height="9" rx="4" stroke="currentColor" strokeWidth="1.6" fill="none" />
      <rect x="15" y="10" width="11" height="9" rx="4" stroke="currentColor" strokeWidth="1.6" fill="none" />
    </svg>
  );
}
