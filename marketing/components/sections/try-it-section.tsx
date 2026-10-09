"use client";

import { ArrowUpRight } from "@phosphor-icons/react/dist/ssr";
import { Reveal } from "@/components/interactive/reveal";
import { MaskReveal } from "@/components/interactive/mask-reveal";
import { MagneticCta } from "@/components/ui/magnetic-cta";
import { CtaIcon } from "@/components/ui/cta-icon";
import { SecondaryCta } from "@/components/ui/secondary-cta";
import { BOT, PAYS_IN_CIRCLE_USDC, PRIMARY_CTA, SECONDARY_CTA } from "@/lib/data/site";

// What somebody needs before the bot is any use to them, in the order they
// need it, each with the one link that gets it done.
//
// Rendered only when the site has been told where the bot is: see
// lib/data/site.ts. A set of steps that ends at "add the bot" with no bot to
// add is a dead end with extra reading.
//
// A numbered list, and deliberately so. "How it works" above dropped its
// numbering because position on its line already said the order. This is a
// different thing: instructions a person follows with another tab open, who
// needs to find their place again when they come back.
//
// Every link here leaves the site, to something stelfin does not run. Each is
// named for what it is, so nobody arrives at a wallet's download page having
// been told only "click here".
interface Step {
  id: string;
  title: string;
  body: string;
  links: { href: string; label: string }[];
}

const STEPS: Step[] = [
  {
    id: "wallet",
    title: "Install Freighter, and switch it to Testnet",
    body: "A wallet that lives in your browser. Your key stays in it. stelfin asks it to sign, and never sees the key.",
    links: [{ href: "https://www.freighter.app/", label: "freighter.app" }],
  },
  {
    id: "fund",
    title: "Fund the wallet with test XLM",
    body: "Paste your address into Stellar Lab and Friendbot creates the account with 10,000 XLM in it. It is worth nothing, which is the point.",
    links: [{ href: "https://lab.stellar.org/account/fund", label: "Stellar Lab, fund account" }],
  },
  ...(PAYS_IN_CIRCLE_USDC
    ? [
        {
          id: "usdc",
          title: "Get test USDC",
          body: "Add USDC as an asset in Freighter first, so the wallet can hold it. Then ask Circle's faucet for some, on Stellar Testnet.",
          links: [{ href: "https://faucet.circle.com/", label: "Circle's testnet faucet" }],
        },
      ]
    : []),
  {
    id: "bot",
    title: "Add stelfin to a group, and run /setup",
    body: "An admin of the group runs /setup once. Everyone who will sign then runs /link, and proves a wallet is theirs by signing a challenge that cannot move anything.",
    links: [
      ...(BOT.telegram ? [{ href: BOT.telegram.addToGroup, label: "Add to a Telegram group" }] : []),
      ...(BOT.discord ? [{ href: BOT.discord.addToServer, label: "Add to a Discord server" }] : []),
    ],
  },
];

export function TryItSection() {
  return (
    <section id="try-it" className="bg-bg py-20 md:py-28">
      <div className="mx-auto grid max-w-[1100px] gap-14 px-6 lg:grid-cols-[minmax(0,6fr)_minmax(0,7fr)] lg:gap-16">
        <div className="lg:sticky lg:top-28 lg:self-start">
          <Reveal>
            <span className="font-mono text-xs uppercase tracking-[0.2em] text-fg-subtle">Try it on testnet</span>
          </Reveal>
          <MaskReveal
            as="h2"
            text="Run a treasury with money that isn't real"
            accent="money that isn't real"
            className="mt-4 text-[30px] font-medium leading-[1.06] tracking-[-0.02em] text-fg md:text-[40px]"
          />
          <Reveal delay={0.1}>
            <p className="mt-6 max-w-[42ch] text-base leading-relaxed text-fg-muted md:text-lg">
              stelfin runs on Stellar&rsquo;s test network. Everything works the way it would with real
              funds, and none of it is worth anything, so a group can find out how it signs before
              it matters.
            </p>
          </Reveal>
          <Reveal delay={0.2}>
            <div className="mt-9 flex flex-wrap items-center gap-4">
              <MagneticCta
                href={PRIMARY_CTA.href}
                target="_blank"
                rel="noopener"
                className="flex items-center gap-2.5 text-base transition-transform duration-300 hover:-translate-y-0.5"
              >
                <CtaIcon kind={PRIMARY_CTA.kind} />
                <span>{PRIMARY_CTA.label}</span>
              </MagneticCta>
              {SECONDARY_CTA && <SecondaryCta cta={SECONDARY_CTA} />}
            </div>
          </Reveal>
        </div>

        <ol className="border-t border-line">
          {STEPS.map((step, i) => (
            <li key={step.id} className="border-b border-line">
              <Reveal delay={i * 0.08}>
                <div className="grid grid-cols-[2.25rem_minmax(0,1fr)] gap-x-4 py-7 md:grid-cols-[3rem_minmax(0,1fr)] md:py-9">
                  <span aria-hidden="true" className="pt-1 font-mono text-sm tabular-nums text-fg-subtle">
                    {i + 1}
                  </span>
                  <div>
                    <h3 className="text-lg font-medium tracking-tight text-fg md:text-xl">{step.title}</h3>
                    <p className="mt-2 max-w-[58ch] text-[15px] leading-relaxed text-fg-muted md:text-base">
                      {step.body}
                    </p>
                    <ul className="mt-4 flex flex-wrap gap-x-6 gap-y-2">
                      {step.links.map((link) => (
                        <li key={link.href}>
                          <a
                            href={link.href}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="group inline-flex items-center gap-1.5 text-[15px] font-medium text-accent underline-offset-4 hover:underline md:text-base"
                          >
                            {link.label}
                            <ArrowUpRight
                              size={16}
                              weight="bold"
                              aria-hidden="true"
                              className="transition-transform duration-300 group-hover:-translate-y-0.5 group-hover:translate-x-0.5"
                            />
                            <span className="sr-only">(opens in a new tab)</span>
                          </a>
                        </li>
                      ))}
                    </ul>
                  </div>
                </div>
              </Reveal>
            </li>
          ))}
        </ol>
      </div>
    </section>
  );
}
