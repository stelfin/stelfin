"use client";

import { motion, useScroll, useTransform } from "framer-motion";
import { useRef } from "react";
import { MagneticCta } from "@/components/ui/magnetic-cta";
import { WorldConnections } from "@/components/ui/world-connections";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { fadeUp, stagger } from "@/lib/animation/variants";
import { SITE } from "@/lib/data/site";

export function Hero() {
  const ref = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start start", "end start"] });

  const headlineY = useTransform(scrollYProgress, [0, 1], [0, -40]);
  const mapOpacity = useTransform(scrollYProgress, [0, 1], [1, 0.3]);

  return (
    <section ref={ref} className="relative overflow-hidden px-[10px] pt-[14px] sm:px-[72px] lg:h-screen">
      <motion.div aria-hidden style={{ opacity: mapOpacity }} className="pointer-events-none absolute inset-0 -z-10">
        <WorldConnections />
      </motion.div>
      <div aria-hidden className="bg-grain pointer-events-none absolute inset-0 -z-10" />

      <div className="mx-auto flex h-full max-w-[900px] flex-col items-center justify-center px-6 py-24 text-center lg:py-0">
        <motion.div style={{ y: headlineY }} initial="hidden" animate="visible" className="flex flex-col items-center">
          <MaskReveal
            as="h1"
            trigger="mount"
            delay={0}
            text="Run your DAO treasury from the chat you already use"
            accent="from the chat you already use"
            className="justify-center text-center text-[44px] font-medium leading-[1.0] tracking-[-0.02em] text-ink-900 sm:text-[64px] md:text-[76px] lg:text-[92px]"
          />

          <motion.p variants={fadeUp(0.5)} className="mt-7 max-w-md text-base leading-relaxed text-ink-700 md:text-lg">
            Non-custodial Stellar operations, in Telegram and Discord.
          </motion.p>

          <motion.div variants={fadeUp(0.62)} className="mt-9 flex flex-wrap items-center justify-center gap-5 md:mt-11 md:gap-6">
            <MagneticCta
              href={SITE.ctaHref}
              target="_blank"
              rel="noopener"
              className="group relative isolate flex items-center gap-2.5 overflow-hidden text-base shadow-accent transition-[transform,box-shadow] duration-300 hover:-translate-y-0.5 hover:shadow-[0_16px_44px_-8px_rgb(20_113_61/0.6)] md:!px-8 md:!py-4 md:text-lg"
            >
              <span
                aria-hidden
                className="pointer-events-none absolute inset-0 -z-10 -translate-x-full bg-gradient-to-r from-transparent via-white/30 to-transparent transition-transform duration-700 ease-out group-hover:translate-x-full"
              />
              <GitHubIcon />
              <span className="text-white">{SITE.ctaLabel}</span>
            </MagneticCta>

            <a
              href="#how-it-works"
              data-cursor="grow"
              className="group flex items-center gap-2 text-base font-medium text-ink-900 underline-offset-4 transition-colors hover:underline md:text-lg"
            >
              See how it works
              <span aria-hidden="true" className="grid h-5 w-5 place-items-center transition-transform duration-300 group-hover:translate-x-1">
                <svg viewBox="0 0 12 12" className="h-3 w-3">
                  <path d="M2 6h8m0 0L6 2m4 4L6 10" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" fill="none" />
                </svg>
              </span>
            </a>
          </motion.div>

          <motion.div variants={stagger(0.08, 0.8)} className="mt-12 flex flex-wrap items-center justify-center gap-x-6 gap-y-3 text-[15px] text-ink-700 md:text-base">
            <TrustItem icon={<LockGlyph />} label="Non-custodial" />
            <TrustItem icon={<SponsorGlyph />} label="Sponsored — no XLM needed" />
            <TrustItem icon={<CheckGlyph />} label="Every claim verified" />
          </motion.div>
        </motion.div>
      </div>
    </section>
  );
}

function TrustItem({ icon, label }: { icon: React.ReactNode; label: string }) {
  return (
    <motion.span variants={fadeUp(0, 8)} className="flex items-center gap-2">
      {icon}
      {label}
    </motion.span>
  );
}

function GitHubIcon() {
  return (
    <svg viewBox="0 0 24 24" width={20} height={20} fill="white" aria-hidden="true">
      <path d="M12 .5C5.73.5.5 5.73.5 12a11.5 11.5 0 0 0 7.86 10.92c.58.1.79-.25.79-.56v-2.16c-3.2.7-3.88-1.37-3.88-1.37-.53-1.34-1.29-1.7-1.29-1.7-1.05-.72.08-.7.08-.7 1.16.08 1.77 1.2 1.77 1.2 1.03 1.77 2.71 1.26 3.37.96.1-.75.4-1.26.73-1.55-2.55-.29-5.24-1.28-5.24-5.7 0-1.26.45-2.29 1.19-3.1-.12-.29-.52-1.46.11-3.05 0 0 .97-.31 3.18 1.18a11 11 0 0 1 5.79 0c2.2-1.49 3.17-1.18 3.17-1.18.63 1.59.24 2.76.12 3.05.74.81 1.19 1.84 1.19 3.1 0 4.43-2.7 5.4-5.27 5.69.42.36.78 1.06.78 2.14v3.17c0 .31.21.67.8.56A11.5 11.5 0 0 0 23.5 12C23.5 5.73 18.27.5 12 .5Z" />
    </svg>
  );
}
function LockGlyph() {
  return (
    <svg viewBox="0 0 20 20" width={16} height={16} fill="none" aria-hidden="true">
      <rect x="4" y="8.5" width="12" height="8" rx="1.5" stroke="currentColor" strokeWidth="1.5" />
      <path d="M6.5 8.5V6a3.5 3.5 0 0 1 7 0v2.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  );
}
function SponsorGlyph() {
  return (
    <svg viewBox="0 0 20 20" width={16} height={16} fill="none" aria-hidden="true">
      <circle cx="10" cy="10" r="7" stroke="currentColor" strokeWidth="1.5" />
      <path d="M10 6.5v7M7.5 8.3c0-1 .9-1.8 2.5-1.8s2.5.7 2.5 1.6c0 2.1-5 1.3-5 3.4 0 .9 1 1.6 2.5 1.6s2.5-.6 2.5-1.6" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
    </svg>
  );
}
function CheckGlyph() {
  return (
    <svg viewBox="0 0 20 20" width={16} height={16} fill="none" aria-hidden="true">
      <path d="m5 10.5 3.2 3.2L15 7" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
