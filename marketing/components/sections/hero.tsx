"use client";

import { motion, useScroll, useTransform } from "framer-motion";
import { useRef } from "react";
import { MagneticCta } from "@/components/ui/magnetic-cta";
import { PhoneFrame } from "@/components/ui/phone-frame";
import { LiveChatThread } from "@/components/illustrations/live-chat-thread";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { EASE, fadeUp, stagger } from "@/lib/animation/variants";
import { useTilt } from "@/lib/hooks/use-tilt";
import { SITE } from "@/lib/data/site";

export function Hero() {
  const ref = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start start", "end start"] });

  const phoneY = useTransform(scrollYProgress, [0, 1], [0, -120]);
  const headlineY = useTransform(scrollYProgress, [0, 1], [0, -40]);
  const auroraY = useTransform(scrollYProgress, [0, 1], [0, 140]);
  const cueOpacity = useTransform(scrollYProgress, [0, 0.12], [1, 0]);

  return (
    <section ref={ref} className="relative overflow-hidden px-[10px] pt-[14px] sm:px-[72px] lg:h-screen">
      <motion.div aria-hidden style={{ y: auroraY }} className="pointer-events-none absolute inset-0 -z-10">
        <div className="animate-drift-slow absolute -left-24 top-24 h-[420px] w-[420px] rounded-full bg-accent-300/45 blur-[90px]" />
        <div className="animate-drift-slow absolute right-[-6rem] top-1/3 h-[360px] w-[360px] rounded-full bg-accent-500/25 blur-[100px]" style={{ animationDelay: "-5s" }} />
        <div className="animate-drift-slow absolute bottom-0 left-1/3 h-[280px] w-[280px] rounded-full bg-accent-300/25 blur-[85px]" style={{ animationDelay: "-8s" }} />
      </motion.div>
      <div aria-hidden className="bg-grain pointer-events-none absolute inset-0 -z-10" />

      <div className="mx-auto grid h-full max-w-[1400px] items-center gap-8 px-6 lg:grid-cols-12 lg:gap-12">
        <motion.div style={{ y: headlineY }} initial="hidden" animate="visible" className="lg:col-span-7 lg:max-w-[720px]">
          <motion.div
            variants={fadeUp(0)}
            className="mb-5 inline-flex items-center gap-2 rounded-full border border-ink-900/10 bg-white/60 px-3.5 py-1.5 text-xs font-medium text-ink-700 backdrop-blur-sm md:text-sm"
          >
            <span className="relative flex h-1.5 w-1.5">
              <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-amber-500 opacity-75" />
              <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-amber-500" />
            </span>
            Testnet demo — not yet accepting real funds
          </motion.div>

          <MaskReveal
            as="h1"
            trigger="mount"
            delay={0.12}
            text="Send stablecoins on Stellar, right from WhatsApp"
            accent="right from WhatsApp"
            className="text-[40px] font-medium leading-[1.02] tracking-[-0.02em] text-ink-900 sm:text-[52px] lg:max-w-[640px] lg:text-[64px] xl:text-[72px]"
          />

          <motion.p variants={fadeUp(0.55)} className="mt-7 max-w-xl text-base leading-relaxed text-ink-700 md:text-xl">
            stelfin is a non-custodial stablecoin wallet you talk to instead of
            open. No app to install, no seed phrase to guard — your key stays
            on your device, every time.
          </motion.p>

          <motion.div variants={fadeUp(0.68)} className="mt-9 flex flex-wrap items-center gap-5 md:mt-11 md:gap-6">
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

          <motion.div variants={stagger(0.08, 0.85)} className="mt-12 hidden flex-wrap items-center gap-x-6 gap-y-3 text-[15px] text-ink-700 lg:flex md:text-base">
            <TrustItem icon={<LockGlyph />} label="Non-custodial" />
            <TrustItem icon={<SponsorGlyph />} label="Sponsored — no XLM needed" />
            <TrustItem icon={<CheckGlyph />} label="Every claim verified" />
          </motion.div>
        </motion.div>

        <motion.div
          style={{ y: phoneY }}
          initial={{ opacity: 0, y: 40 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 1, delay: 0.4, ease: EASE }}
          className="relative lg:col-span-5"
        >
          <HeroPhone />
        </motion.div>

        <motion.div
          variants={stagger(0.08, 0.2)}
          initial="hidden"
          animate="visible"
          className="flex flex-wrap items-center justify-center gap-4 text-center text-[15px] text-ink-700 lg:hidden"
        >
          <TrustItem icon={<LockGlyph />} label="Non-custodial" />
          <TrustItem icon={<SponsorGlyph />} label="Sponsored — no XLM needed" />
          <TrustItem icon={<CheckGlyph />} label="Every claim verified" />
        </motion.div>
      </div>

      <motion.div style={{ opacity: cueOpacity }} className="pointer-events-none absolute inset-x-0 bottom-6 hidden flex-col items-center gap-2 text-ink-400 lg:flex">
        <span className="text-[10px] uppercase tracking-[0.22em]">Scroll</span>
        <span className="flex h-7 w-[18px] justify-center rounded-full border border-ink-300/70 pt-1.5">
          <motion.span
            className="h-1.5 w-1 rounded-full bg-ink-400"
            animate={{ y: [0, 6, 0], opacity: [1, 0.3, 1] }}
            transition={{ duration: 1.6, repeat: Infinity, ease: "easeInOut" }}
          />
        </span>
      </motion.div>
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

function HeroPhone() {
  const { rotateX, rotateY, onMouseMove, onMouseLeave } = useTilt({ max: 7 });

  return (
    <div className="relative mx-auto w-fit">
      <motion.div
        initial={{ opacity: 0, y: 20, x: -10 }}
        animate={{ opacity: 1, y: 0, x: 0 }}
        transition={{ duration: 0.8, delay: 1.2, ease: EASE }}
        className="animate-drift-slow absolute -left-20 top-28 z-10 hidden w-40 items-center gap-3 rounded-2xl bg-white p-3 shadow-card ring-1 ring-ink-200/30 lg:-left-16 lg:flex xl:-left-20"
      >
        <div className="grid h-9 w-9 shrink-0 place-items-center rounded-full bg-accent-50 text-xs font-semibold text-accent-600">
          <LockGlyph />
        </div>
        <div className="flex-1 leading-tight">
          <p className="text-[10px] uppercase tracking-wider text-ink-400">Signed by</p>
          <p className="font-sans text-base font-semibold leading-none text-ink-900">Your device</p>
          <p className="text-[10px] text-ink-500">never the server</p>
        </div>
      </motion.div>

      <motion.div
        initial={{ opacity: 0, y: 20, x: 10 }}
        animate={{ opacity: 1, y: 0, x: 0 }}
        transition={{ duration: 0.8, delay: 1.4, ease: EASE }}
        className="animate-drift-slow absolute -right-8 bottom-8 z-10 hidden w-44 rounded-2xl bg-ink-900 p-4 text-surface-50 shadow-card ring-1 ring-white/10 lg:-bottom-2 lg:-right-12 lg:block"
        style={{ animationDelay: "-3s" }}
      >
        <p className="font-sans text-3xl font-semibold leading-none">0</p>
        <p className="mt-2 text-[11px] leading-tight text-surface-50/60">XLM you ever need to hold</p>
      </motion.div>

      <motion.div
        onMouseMove={onMouseMove}
        onMouseLeave={onMouseLeave}
        style={{ rotateX, rotateY, transformPerspective: 1200, transformStyle: "preserve-3d" }}
        className="will-change-transform"
      >
        <PhoneFrame className="lg:!w-[340px] xl:!w-[368px]">
          <LiveChatThread />
        </PhoneFrame>
      </motion.div>

      <div aria-hidden className="absolute -bottom-6 left-1/2 -z-10 h-10 w-[70%] -translate-x-1/2 rounded-[100%] bg-ink-900/15 blur-2xl" />
    </div>
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
