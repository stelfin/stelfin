"use client";

import { motion, useScroll, useTransform } from "framer-motion";
import { useRef } from "react";
import { MagneticCta } from "@/components/ui/magnetic-cta";
import { WorldConnections } from "@/components/ui/world-connections";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { CtaIcon } from "@/components/ui/cta-icon";
import { fadeUp } from "@/lib/animation/variants";
import { PRIMARY_CTA } from "@/lib/data/site";

export function Hero() {
  const ref = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start start", "end start"] });

  const headlineY = useTransform(scrollYProgress, [0, 1], [0, -40]);
  const mapOpacity = useTransform(scrollYProgress, [0, 1], [1, 0.3]);

  return (
    // min-h-[100dvh], not h-screen: on iOS Safari the address bar collapsing
    // changes vh mid-scroll, so an h-screen hero visibly jumps.
    <section
      ref={ref}
      className="relative overflow-hidden px-4 pt-[14px] sm:px-[72px] lg:min-h-[100dvh]"
    >
      <motion.div aria-hidden style={{ opacity: mapOpacity }} className="pointer-events-none absolute inset-0 -z-10">
        <WorldConnections />
      </motion.div>
      <div aria-hidden className="bg-grain pointer-events-none absolute inset-0 -z-10" />

      <div className="mx-auto flex h-full max-w-[1040px] flex-col items-center justify-center px-2 py-24 text-center sm:px-6 lg:min-h-[100dvh] lg:py-0">
        <motion.div style={{ y: headlineY }} initial="hidden" animate="visible" className="flex flex-col items-center">
          {/* Scaled and measured to land in two lines, not three. Ten words at
              92px inside a 900px column wrapped to three and left "use" alone
              on the last one; a hero headline that orphans its final word is a
              font-size decision, not a copy problem. */}
          <MaskReveal
            as="h1"
            trigger="mount"
            delay={0}
            text="Run your DAO treasury from the chat you already use"
            accent="from the chat you already use"
            className="justify-center text-center text-[34px] font-medium leading-[1.05] tracking-[-0.02em] text-fg sm:text-[44px] md:text-[54px] lg:text-[60px]"
          />

          <motion.p variants={fadeUp(0.5)} className="mt-7 max-w-md text-base leading-relaxed text-fg-muted md:text-lg">
            Propose a payment in the group chat. Signers approve it on their own devices.
          </motion.p>

          <motion.div variants={fadeUp(0.62)} className="mt-9 flex flex-wrap items-center justify-center gap-5 md:mt-11 md:gap-6">
            <MagneticCta
              href={PRIMARY_CTA.href}
              target="_blank"
              rel="noopener"
              className="group relative isolate flex items-center gap-2.5 overflow-hidden text-base shadow-[var(--shadow-accent)] transition-[transform,box-shadow] duration-300 hover:-translate-y-0.5 hover:shadow-[var(--shadow-accent-lifted)] md:!px-8 md:!py-4 md:text-lg"
            >
              <span
                aria-hidden
                className="pointer-events-none absolute inset-0 -z-10 -translate-x-full bg-gradient-to-r from-transparent via-white/30 to-transparent transition-transform duration-700 ease-out group-hover:translate-x-full"
              />
              <CtaIcon kind={PRIMARY_CTA.kind} />
              <span>{PRIMARY_CTA.label}</span>
            </MagneticCta>

            <a
              href="#how-it-works"
              className="group flex items-center gap-2 text-base font-medium text-fg underline-offset-4 transition-colors hover:underline md:text-lg"
            >
              See how it works
              <span aria-hidden="true" className="grid h-5 w-5 place-items-center transition-transform duration-300 group-hover:translate-x-1">
                <svg viewBox="0 0 12 12" className="h-3 w-3">
                  <path d="M2 6h8m0 0L6 2m4 4L6 10" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" fill="none" />
                </svg>
              </span>
            </a>
          </motion.div>
        </motion.div>
      </div>
    </section>
  );
}
