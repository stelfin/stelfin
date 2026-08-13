"use client";

import { useState, useEffect } from "react";
import Link from "next/link";
import { motion, AnimatePresence, useScroll, useTransform } from "framer-motion";
import { BrandMark } from "@/components/ui/brand-mark";
import { MagneticCta } from "@/components/ui/magnetic-cta";
import { NAV_LINKS, SITE } from "@/lib/data/site";

export function Navbar() {
  const { scrollY } = useScroll();
  const bgOpacity = useTransform(scrollY, [0, 80], [0, 0.85]);
  const borderOpacity = useTransform(scrollY, [0, 80], [0, 0.08]);

  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (!open) return;
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = prev;
    };
  }, [open]);

  useEffect(() => {
    const mq = window.matchMedia("(min-width: 1024px)");
    const onChange = (e: MediaQueryListEvent) => {
      if (e.matches) setOpen(false);
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  return (
    <motion.header
      className="sticky inset-x-0 top-0 z-50 px-[10px] backdrop-blur-md sm:px-[72px]"
      style={{
        backgroundColor: useTransform(bgOpacity, (v) => `rgba(250, 250, 249, ${v})`),
        borderBottom: "1px solid",
        borderColor: useTransform(borderOpacity, (v) => `rgba(14, 18, 15, ${v})`),
      }}
    >
      <div className="mx-auto flex max-w-[1400px] items-center justify-between px-6 py-5">
        <Link href="/" aria-label="stelfin - home" data-cursor="grow">
          <BrandMark />
        </Link>

        <nav aria-label="Primary" className="hidden md:block">
          <ul className="flex items-center gap-10 text-base font-medium text-ink-700">
            {NAV_LINKS.map((link) => (
              <li key={link.href}>
                <a href={link.href} className="group relative transition-colors hover:text-ink-900">
                  {link.label}
                  <span className="absolute -bottom-1 left-0 h-px w-full origin-left scale-x-0 bg-ink-900 transition-transform duration-300 ease-[cubic-bezier(0.16,1,0.3,1)] group-hover:scale-x-100" />
                </a>
              </li>
            ))}
          </ul>
        </nav>

        <div className="hidden md:block">
          <MagneticCta href={SITE.whatsappLink} target="_blank" rel="noopener" className="text-base">
            Chat on WhatsApp
          </MagneticCta>
        </div>

        <button
          type="button"
          aria-label={open ? "Close menu" : "Open menu"}
          aria-expanded={open}
          aria-controls="mobile-nav"
          onClick={() => setOpen((v) => !v)}
          className="relative grid h-10 w-10 place-items-center rounded-md text-ink-900 transition-colors hover:bg-ink-900/5 md:hidden"
        >
          <span className="sr-only">Menu</span>
          <span aria-hidden="true" className="relative block h-4 w-5">
            <span className={`absolute left-0 right-0 top-0 h-[2px] rounded-full bg-current transition-transform duration-200 ${open ? "translate-y-[7px] rotate-45" : ""}`} />
            <span className={`absolute left-0 right-0 top-1/2 h-[2px] -translate-y-1/2 rounded-full bg-current transition-opacity duration-200 ${open ? "opacity-0" : "opacity-100"}`} />
            <span className={`absolute left-0 right-0 bottom-0 h-[2px] rounded-full bg-current transition-transform duration-200 ${open ? "-translate-y-[7px] -rotate-45" : ""}`} />
          </span>
        </button>
      </div>

      <AnimatePresence>
        {open && (
          <motion.div
            id="mobile-nav"
            initial={{ opacity: 0, y: -8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -8 }}
            transition={{ duration: 0.18, ease: "easeOut" }}
            className="md:hidden"
          >
            <nav aria-label="Mobile" className="border-t border-ink-900/5 bg-surface-50/95 backdrop-blur-md">
              <ul className="flex flex-col gap-1 px-6 py-4 text-base font-semibold text-ink-700">
                {NAV_LINKS.map((link) => (
                  <li key={link.href}>
                    <a href={link.href} onClick={() => setOpen(false)} className="block rounded-md py-3 transition-colors hover:text-ink-900">
                      {link.label}
                    </a>
                  </li>
                ))}
                <li className="pt-3">
                  <MagneticCta href={SITE.whatsappLink} target="_blank" rel="noopener">
                    Chat on WhatsApp
                  </MagneticCta>
                </li>
              </ul>
            </nav>
          </motion.div>
        )}
      </AnimatePresence>
    </motion.header>
  );
}
