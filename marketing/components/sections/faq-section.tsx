"use client";

import { useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { EASE } from "@/lib/animation/variants";
import { FAQS } from "@/lib/data/faqs";
import type { FaqItem } from "@/lib/data/faqs";

export function FaqSection() {
  const [openId, setOpenId] = useState<string>(FAQS[0]?.id ?? "");

  return (
    <section id="faq" className="bg-surface-50 py-16 lg:py-28">
      <div className="mx-auto max-w-5xl px-6">
        <div className="text-center">
          <Reveal>
            <span className="font-mono text-xs uppercase tracking-[0.2em] text-ink-400">FAQ</span>
          </Reveal>
          <MaskReveal
            as="h2"
            text="Questions, answered honestly."
            accent="honestly."
            className="mt-5 justify-center font-display text-[32px] font-medium leading-[1.04] tracking-[-0.02em] text-ink-900 md:text-[48px]"
          />
        </div>

        <div className="mt-12 grid gap-4 sm:grid-cols-2 md:mt-16">
          {FAQS.map((faq, idx) => (
            <Reveal key={faq.id} delay={idx * 0.05}>
              <FaqCard faq={faq} open={openId === faq.id} onToggle={() => setOpenId((current) => (current === faq.id ? "" : faq.id))} />
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  );
}

function FaqCard({ faq, open, onToggle }: { faq: FaqItem; open: boolean; onToggle: () => void }) {
  return (
    <div
      className={`h-full rounded-2xl border bg-white p-6 transition-colors duration-300 ${
        open ? "border-accent-500/40" : "border-ink-200/70 hover:border-ink-300"
      }`}
    >
      <button
        type="button"
        data-cursor="grow"
        aria-expanded={open}
        aria-controls={`faq-panel-${faq.id}`}
        onClick={onToggle}
        className="flex w-full cursor-pointer items-start justify-between gap-4 text-left"
      >
        <h3 className="text-base font-medium tracking-tight text-ink-900 sm:text-lg">{faq.question}</h3>
        <span
          aria-hidden="true"
          className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-full border text-ink-700 transition-[transform,background-color,border-color,color] duration-500 ease-[cubic-bezier(0.16,1,0.3,1)] ${
            open ? "rotate-45 border-accent-500 bg-accent-500 text-white" : "border-ink-200"
          }`}
        >
          <svg viewBox="0 0 16 16" className="h-3.5 w-3.5">
            <path d="M8 3v10M3 8h10" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
          </svg>
        </span>
      </button>

      <AnimatePresence initial={false}>
        {open && (
          <motion.div
            id={`faq-panel-${faq.id}`}
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: "auto", opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.35, ease: EASE }}
            className="overflow-hidden"
          >
            <p className="pt-4 text-[15px] leading-relaxed text-ink-500">{faq.answer}</p>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  );
}
