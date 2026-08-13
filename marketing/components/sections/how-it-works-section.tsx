"use client";

import { motion, useScroll, useTransform } from "framer-motion";
import { useRef } from "react";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { StaticBubble, ReceiptCard } from "@/components/ui/chat-mockup";

const STEPS = [
  {
    n: "1",
    title: "Message stelfin",
    body: '"send 5,000 to my brother" — plain language, no addresses to copy.',
    visual: (
      <div className="w-full max-w-[220px]">
        <StaticBubble side="out" time="9:14">send 5,000 to my brother</StaticBubble>
      </div>
    ),
  },
  {
    n: "2",
    title: "Confirm on your device",
    body: "A link opens the exact payment for you to sign — amount and recipient, read straight from the transaction, nothing hidden.",
    visual: <ConfirmPreview />,
  },
  {
    n: "3",
    title: "It lands on Stellar",
    body: "Settled on-chain in seconds, sponsored so you never need to hold XLM.",
    visual: (
      <div className="flex w-full max-w-[220px] justify-end">
        <ReceiptCard status="Sent" statusTone="confirmed" amount="5,000.00 USDC" detail="to Brother" time="9:15" />
      </div>
    ),
  },
];

export function HowItWorksSection() {
  const ref = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start end", "start start"] });
  const backgroundColor = useTransform(scrollYProgress, [0, 1], ["#fafaf9", "#eaf7ef"]);

  return (
    <motion.section id="how-it-works" ref={ref} style={{ backgroundColor }} className="relative py-20 md:py-28">
      <div className="mx-auto max-w-[1100px] px-6">
        <div className="max-w-[640px]">
          <Reveal>
            <span className="font-mono text-xs uppercase tracking-[0.2em] text-ink-400">How it works</span>
          </Reveal>
          <MaskReveal
            as="h2"
            text="Three steps, no forms"
            accent="no forms"
            className="mt-4 font-display text-[32px] font-medium leading-[1.04] tracking-[-0.02em] text-ink-900 md:text-[48px]"
          />
          <Reveal delay={0.15}>
            <p className="mt-5 text-base leading-relaxed text-ink-700 md:text-lg">
              From message to money, without ever leaving the chat.
            </p>
          </Reveal>
        </div>

        <div className="mt-14 grid gap-6 md:mt-20 md:grid-cols-3 md:gap-8">
          {STEPS.map((step, i) => (
            <Reveal key={step.n} delay={i * 0.12}>
              <article className="flex h-full flex-col gap-6 rounded-[24px] border border-ink-200/70 bg-white p-6 md:p-7">
                <div className="flex min-h-[140px] items-center justify-center rounded-2xl bg-surface-100 px-4 py-6">
                  {step.visual}
                </div>
                <div>
                  <div className="flex items-center gap-3">
                    <span className="grid h-7 w-7 place-items-center rounded-full bg-accent-500 text-sm font-semibold text-white">
                      {step.n}
                    </span>
                    <h3 className="text-lg font-medium tracking-tight text-ink-900">{step.title}</h3>
                  </div>
                  <p className="mt-3 text-[15px] leading-relaxed text-ink-500">{step.body}</p>
                </div>
              </article>
            </Reveal>
          ))}
        </div>
      </div>
    </motion.section>
  );
}

function ConfirmPreview() {
  return (
    <div className="w-full max-w-[220px] rounded-2xl border border-ink-200 bg-white p-3.5 shadow-sm">
      <p className="text-[10px] uppercase tracking-wider text-ink-400">Confirm this payment</p>
      <p className="mt-1 text-xl font-semibold tabular-nums text-ink-900">5,000.00 <span className="text-sm font-medium text-ink-400">USDC</span></p>
      <p className="mt-0.5 text-[11px] text-ink-500">to Brother</p>
      <div className="mt-3 rounded-lg bg-accent-500 py-1.5 text-center text-[11px] font-semibold text-white">Send</div>
    </div>
  );
}
