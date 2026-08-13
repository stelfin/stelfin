import { BrandMark } from "@/components/ui/brand-mark";
import { SITE } from "@/lib/data/site";

export function Footer() {
  return (
    <footer className="bg-ink-900 py-16 text-surface-50">
      <div className="mx-auto flex max-w-[1400px] flex-col gap-12 px-6 lg:flex-row lg:items-end lg:justify-between">
        <div className="space-y-6">
          <BrandMark />
          <p className="max-w-md text-base leading-relaxed text-surface-50/60 md:text-lg">
            Non-custodial stablecoin payments, by message. Built on Stellar.
          </p>
          <p className="text-base text-surface-50/50">
            Testnet demo — not for real-money use yet.
          </p>
        </div>

        <nav aria-label="Footer">
          <ul className="grid grid-cols-2 gap-x-12 gap-y-4 text-base sm:grid-cols-2">
            <li>
              <a
                href={SITE.githubUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="text-surface-50/60 transition-colors hover:text-surface-50"
              >
                Source on GitHub
              </a>
            </li>
            <li>
              <a
                href={SITE.appUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="text-surface-50/60 transition-colors hover:text-surface-50"
              >
                Open the app
              </a>
            </li>
          </ul>
        </nav>
      </div>

      <div className="mt-12 border-t border-surface-50/10">
        <div className="mx-auto flex max-w-[1400px] items-center justify-between px-6 pt-8 text-sm text-surface-50/40">
          <span>© {new Date().getFullYear()} {SITE.legalName}. All rights reserved.</span>
          <span className="font-mono">testnet</span>
        </div>
      </div>
    </footer>
  );
}
