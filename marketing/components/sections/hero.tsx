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
            text="Send stablecoins on Stellar, right from WhatsApp"
            accent="right from WhatsApp"
            className="justify-center text-center text-[44px] font-medium leading-[1.0] tracking-[-0.02em] text-ink-900 sm:text-[64px] md:text-[76px] lg:text-[92px]"
          />

          <motion.p variants={fadeUp(0.5)} className="mt-7 max-w-md text-base leading-relaxed text-ink-700 md:text-lg">
            A non-custodial wallet you talk to instead of open.
          </motion.p>

          <motion.div variants={fadeUp(0.62)} className="mt-9 flex flex-wrap items-center justify-center gap-5 md:mt-11 md:gap-6">
            <MagneticCta
              href={SITE.whatsappLink}
              target="_blank"
              rel="noopener"
              className="group relative isolate flex items-center gap-2.5 overflow-hidden text-base shadow-accent transition-[transform,box-shadow] duration-300 hover:-translate-y-0.5 hover:shadow-[0_16px_44px_-8px_rgb(20_113_61/0.6)] md:!px-8 md:!py-4 md:text-lg"
            >
              <span
                aria-hidden
                className="pointer-events-none absolute inset-0 -z-10 -translate-x-full bg-gradient-to-r from-transparent via-white/30 to-transparent transition-transform duration-700 ease-out group-hover:translate-x-full"
              />
              <WhatsappIcon />
              <span className="text-white">Chat on WhatsApp</span>
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

function WhatsappIcon() {
  return (
    <svg viewBox="0 0 24 24" width={20} height={20} fill="white" aria-hidden="true">
      <path d="M12.04 2C6.58 2 2.13 6.45 2.13 11.91c0 1.79.47 3.47 1.29 4.93L2 22l5.29-1.39a9.9 9.9 0 0 0 4.75 1.21h.01c5.46 0 9.9-4.45 9.9-9.91C21.96 6.45 17.5 2 12.04 2Zm5.79 14.06c-.24.68-1.19 1.24-1.94 1.4-.52.11-1.19.2-3.47-.75-2.9-1.2-4.77-4.15-4.92-4.34-.14-.19-1.18-1.57-1.18-2.99 0-1.42.74-2.11 1-2.4.26-.29.57-.36.76-.36.19 0 .38 0 .55.01.18.01.42-.07.65.5.24.58.82 2 .89 2.15.07.15.12.32.02.51-.09.19-.14.31-.28.48-.14.17-.29.37-.42.5-.14.14-.28.29-.12.57.16.28.71 1.17 1.52 1.9 1.04.94 1.92 1.23 2.2 1.37.28.14.44.12.61-.07.17-.19.71-.83.9-1.11.19-.28.38-.24.65-.14.27.1 1.7.8 1.99.95.29.14.48.21.55.33.07.12.07.68-.17 1.36Z" />
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
