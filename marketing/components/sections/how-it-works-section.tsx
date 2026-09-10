"use client";

import { motion, useScroll, useTransform } from "framer-motion";
import { useRef } from "react";
import { ChatTeardropText, DeviceMobile, ShieldCheck } from "@phosphor-icons/react/dist/ssr";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";

// Treasury scale, not personal scale. These used to read "send 5,000 to my
// brother" and "Confirm on your device" — a single person moving their own
// money, which is the product stelfin was before the rebuild. What it does now
// is put a proposal in front of a group and collect signatures.
//
// No "Step 1 / Step 2" numbering: the position on the line already says which
// comes first, and an enumerated label adds nothing the reader cannot see.
const STEPS = [
  {
    id: "propose",
    title: "Someone proposes",
    body: "A member asks in the group chat. stelfin turns it into a transaction.",
    icon: <ChatTeardropText size={28} weight="light" />,
  },
  {
    id: "approve",
    title: "Signers approve",
    body: "Each one checks it in their own browser, on their own device.",
    icon: <DeviceMobile size={28} weight="light" />,
  },
  {
    id: "settle",
    title: "It settles on Stellar",
    body: "Once it has the weight the treasury requires, and not before.",
    icon: <ShieldCheck size={28} weight="light" />,
  },
];

export function HowItWorksSection() {
  const lineRef = useRef<HTMLDivElement | null>(null);
  const { scrollYProgress: lineProgress } = useScroll({ target: lineRef, offset: ["start 65%", "end 65%"] });
  const lineScale = useTransform(lineProgress, [0, 1], [0, 1]);

  return (
    // The scroll-driven background tint that used to live here is gone. The page
    // gets exactly one theme inversion, and it belongs to the Security section;
    // a second tint here made three flips on one page, which under a real dark
    // mode stops reading as narrative and starts reading as a bug.
    <section id="how-it-works" className="relative bg-bg py-20 md:py-28">
      <div className="mx-auto max-w-[1000px] px-6">
        <div className="mx-auto max-w-[560px] text-center">
          <Reveal>
            <span className="font-mono text-xs uppercase tracking-[0.2em] text-fg-subtle">How it works</span>
          </Reveal>
          <MaskReveal
            as="h2"
            text="Three steps, no forms"
            accent="no forms"
            className="mt-4 justify-center text-center text-[32px] font-medium leading-[1.04] tracking-[-0.02em] text-fg md:text-[48px]"
          />
        </div>

        <div ref={lineRef} className="relative mt-20 md:mt-28">
          {/* The connecting line: a static track plus a scroll-filled overlay
              that draws left to right (desktop) as the section scrolls by.
              Motivated, not decorative: the fill is the proposal advancing
              through the three states, which is the section's whole claim. */}
          <div className="absolute left-[10%] right-[10%] top-8 hidden h-[2px] -translate-y-1/2 bg-line md:block" />
          <motion.div
            style={{ scaleX: lineScale }}
            className="absolute left-[10%] right-[10%] top-8 hidden h-[2px] origin-left -translate-y-1/2 bg-accent md:block"
          />
          {/* Mobile: the same idea, vertical. */}
          <div className="absolute bottom-0 left-8 top-0 w-[2px] bg-line md:hidden" />
          <motion.div
            style={{ scaleY: lineScale }}
            className="absolute bottom-0 left-8 top-0 w-[2px] origin-top bg-accent md:hidden"
          />

          <div className="relative grid gap-14 md:grid-cols-3 md:gap-8">
            {STEPS.map((step, i) => (
              <Reveal key={step.id} delay={i * 0.15}>
                <div className="flex items-start gap-5 md:flex-col md:items-center md:text-center">
                  <div className="relative z-10 flex h-16 w-16 flex-none items-center justify-center rounded-full border-2 border-accent bg-bg-raised text-accent shadow-soft">
                    {step.icon}
                  </div>
                  <div className="md:mt-6">
                    <h3 className="text-lg font-medium tracking-tight text-fg">{step.title}</h3>
                    <p className="mt-1.5 max-w-[240px] text-[15px] leading-relaxed text-fg-subtle md:mx-auto">
                      {step.body}
                    </p>
                  </div>
                </div>
              </Reveal>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}
