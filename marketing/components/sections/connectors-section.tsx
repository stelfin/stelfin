"use client";

import { motion } from "framer-motion";
import { FileXls, Robot, UsersThree } from "@phosphor-icons/react/dist/ssr";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { fadeUp, stagger } from "@/lib/animation/variants";

// "A connector can propose; only a human can approve" is the sharpest sentence
// in the repository's own README and it appeared nowhere on the site.
//
// A left-to-right flow rather than cards or a split: the whole point is the
// direction of authority, and the one place it stops. The gate is the section.
const FLOW = [
  {
    id: "source",
    icon: <FileXls size={24} weight="light" />,
    label: "A spreadsheet",
    body: "Payroll in a Google Sheet, read through a credential sealed to one org and one connector.",
  },
  {
    id: "agent",
    icon: <Robot size={24} weight="light" />,
    label: "Or an agent",
    body: "Any MCP server the DAO registers, served only that workspace's own data and nothing else.",
  },
  {
    id: "humans",
    icon: <UsersThree size={24} weight="light" />,
    label: "Still your signers",
    body: "Whatever proposed it, the same people approve it, on the same page, with the same weights.",
  },
];

export function ConnectorsSection() {
  return (
    <section id="connectors" className="bg-bg py-20 md:py-28">
      <div className="mx-auto max-w-[1100px] px-6">
        <div className="max-w-[720px]">
          <MaskReveal
            as="h2"
            text="A connector can propose. Only a human can approve."
            accent="Only a human can approve."
            className="text-[32px] font-medium leading-[1.06] tracking-[-0.02em] text-fg md:text-[44px]"
          />
          <Reveal delay={0.15}>
            <p className="mt-6 max-w-xl text-base leading-relaxed text-fg-muted md:text-lg">
              Spreadsheets, models and trading venues can all put work in front
              of the treasury. None of them can move it.
            </p>
          </Reveal>
        </div>

        {/* A flow, not a card grid. The trust band directly above the fold is
            already a three-cell hairline grid, and repeating that shape here
            would make two different arguments look like the same furniture.
            Unboxed items joined by arrows also happen to be the honest picture:
            authority travels left to right and stops at the third step. */}
        <motion.div
          initial="hidden"
          whileInView="visible"
          viewport={{ once: true, amount: 0.2 }}
          variants={stagger(0.1, 0)}
          className="mt-14 flex flex-col gap-10 md:mt-20 md:flex-row md:items-start md:gap-10"
        >
          {FLOW.map((step, i) => {
            const isGate = i === FLOW.length - 1;
            return (
              <motion.div key={step.id} variants={fadeUp(0, 14)} className="relative flex-1">
                {/* Absolute, not in flow. In the flex column the arrow was a
                    block above the content, so items two and three started
                    lower than item one and the row's baselines did not line up.
                    Out of flow, all three tops agree. */}
                {i > 0 && (
                  <span
                    aria-hidden="true"
                    className="absolute -left-5 top-1.5 hidden text-line-strong md:block"
                  >
                    <svg viewBox="0 0 24 12" className="h-3 w-6">
                      <path
                        d="M0 6h20m0 0-5-4m5 4-5 4"
                        stroke="currentColor"
                        strokeWidth="1.4"
                        fill="none"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                      />
                    </svg>
                  </span>
                )}
                <div className="min-w-0">
                  <div className="flex items-center gap-3">
                    {/* The gate carries the accent and the two before it do not.
                        One coloured thing in a row of three is the section's
                        whole argument, made without a word. */}
                    <span className={isGate ? "text-accent" : "text-fg-subtle"} aria-hidden="true">
                      {step.icon}
                    </span>
                    <h3 className="text-base font-medium tracking-tight text-fg">{step.label}</h3>
                  </div>
                  <p className="mt-3 max-w-xs text-[15px] leading-relaxed text-fg-subtle">{step.body}</p>
                </div>
              </motion.div>
            );
          })}
        </motion.div>
      </div>
    </section>
  );
}
