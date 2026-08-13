"use client";

import { motion, useScroll, useTransform } from "framer-motion";
import { useRef } from "react";
import { MagneticCta } from "@/components/ui/magnetic-cta";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { Reveal } from "@/components/interactive/reveal";
import { SITE } from "@/lib/data/site";

export function ClosingCta() {
  const ref = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start end", "end start"] });
  const scale = useTransform(scrollYProgress, [0, 0.5, 1], [0.96, 1, 1.02]);

  return (
    <section
      ref={ref}
      className="relative px-3 py-[60px] md:px-[72px]"
      style={{ background: "linear-gradient(160deg, #0e120f 0%, #10331e 60%, #14713d 130%)" }}
    >
      <div className="rounded-[32px] bg-white px-[20px] py-[80px] md:py-[120px]">
        <motion.div style={{ scale }} className="flex flex-col items-center">
          <MaskReveal
            as="h2"
            text="Open WhatsApp. Send a message. That's it."
            accent="Send a message."
            className="max-w-[820px] justify-center text-center text-[36px] font-semibold leading-[1.02] tracking-[-0.02em] text-ink-900 sm:text-[52px] lg:text-[68px]"
          />

          <Reveal delay={0.15}>
            <p className="mx-auto mt-6 max-w-lg text-center text-lg leading-relaxed text-ink-700 md:text-xl">
              No signup form, no seed phrase to write down. Say hello and
              stelfin walks you through the rest.
            </p>
          </Reveal>

          <Reveal delay={0.3}>
            <div className="mx-auto mt-12 flex w-fit flex-wrap items-center justify-center gap-5">
              <MagneticCta
                href={SITE.whatsappLink}
                target="_blank"
                rel="noopener"
                className="group relative isolate overflow-hidden rounded-full !bg-ink-900 text-base text-white transition-transform duration-300 hover:-translate-y-0.5 md:!px-9 md:!py-4 md:text-lg"
              >
                <span
                  aria-hidden
                  className="pointer-events-none absolute inset-0 -z-10 -translate-x-full bg-gradient-to-r from-transparent via-white/25 to-transparent transition-transform duration-700 ease-out group-hover:translate-x-full"
                />
                Chat on WhatsApp
              </MagneticCta>
            </div>
          </Reveal>
        </motion.div>
      </div>
    </section>
  );
}
