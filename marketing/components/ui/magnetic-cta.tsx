"use client";

import type { ComponentProps } from "react";
import { useMagnetic } from "@/lib/hooks/use-magnetic";
import { cn } from "@/lib/utils/cn";

type MagneticCtaProps = ComponentProps<"a">;

export function MagneticCta({ className, children, ...props }: MagneticCtaProps) {
  const ref = useMagnetic<HTMLSpanElement>({ strength: 18, radius: 100 });

  return (
    <span ref={ref}>
      <a
        {...props}
       
        className={cn(
          // bg-accent / text-on-accent, not accent-500 / white: in dark mode the
          // accent lightens to #4ec27f, and white on that is 2.25:1 — a CTA
          // nobody with normal vision can read comfortably and that fails AA
          // outright. The semantic pair carries the correct ink in both modes.
          "gap-2 rounded-control bg-accent px-3 py-2 text-sm font-medium text-on-accent transition-colors hover:bg-accent-hover md:px-6 md:py-3.5 md:text-base",
          className,
        )}
      >
        {children}
      </a>
    </span>
  );
}
