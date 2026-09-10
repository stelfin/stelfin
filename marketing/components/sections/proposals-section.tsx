"use client";

import { motion } from "framer-motion";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { fadeUp, stagger } from "@/lib/animation/variants";

// The section the site was missing entirely.
//
// Proposals, signers, weights and thresholds are most of what the product
// actually does, and none of it appeared anywhere on the page. A visitor who
// ran a multisig treasury could read the whole site and never learn that
// stelfin understood multisig.
//
// Asymmetric split rather than another centred block: this is the fourth
// section, and the three before it are centred, banded and centred again.
const STATES = [
  {
    weight: "0 of 3",
    label: "Proposed",
    body: "A member asks in the chat. stelfin builds the transaction and posts it for the signers.",
  },
  {
    weight: "2 of 3",
    label: "Collecting signatures",
    body: "Each signer opens their own link and approves on their own device. Anyone can see who has signed and who has not.",
  },
  {
    weight: "3 of 3",
    label: "Submitted",
    body: "Only once it carries the weight the treasury's own policy requires. Never before, and never by stelfin alone.",
  },
];

export function ProposalsSection() {
  return (
    <section id="proposals" className="bg-bg-sunken py-20 md:py-28">
      <div className="mx-auto grid max-w-[1100px] gap-14 px-6 md:grid-cols-[minmax(0,0.85fr)_minmax(0,1fr)] md:gap-20">
        <div className="md:sticky md:top-32 md:self-start">
          <MaskReveal
            as="h2"
            text="Your treasury's rules, not ours"
            accent="not ours"
            className="text-[32px] font-medium leading-[1.04] tracking-[-0.02em] text-fg md:text-[44px]"
          />
          <Reveal delay={0.15}>
            <p className="mt-6 max-w-md text-base leading-relaxed text-fg-muted md:text-lg">
              stelfin reads the signing weights off your Stellar account and
              collects approvals against them. A single signer, an M-of-N
              multisig, or a Soroban smart account all work the same way,
              because the network decides what counts, not the bot.
            </p>
          </Reveal>
        </div>

        <motion.ol
          initial="hidden"
          whileInView="visible"
          viewport={{ once: true, amount: 0.2 }}
          variants={stagger(0.12, 0)}
          className="relative"
        >
          {STATES.map((state) => (
            <motion.li
              key={state.label}
              variants={fadeUp(0, 16)}
              className="border-t border-line py-8 last:border-b md:py-10"
            >
              <div className="flex items-baseline gap-4">
                <span className="font-mono text-sm tabular-nums text-accent">{state.weight}</span>
                <h3 className="text-lg font-medium tracking-tight text-fg md:text-xl">{state.label}</h3>
              </div>
              <p className="mt-2.5 max-w-lg text-[15px] leading-relaxed text-fg-subtle md:text-base">
                {state.body}
              </p>
            </motion.li>
          ))}
        </motion.ol>
      </div>
    </section>
  );
}
