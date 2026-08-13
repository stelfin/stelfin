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
        data-cursor="grow"
        className={cn(
          "gap-2 rounded-md px-3 py-2 text-sm font-medium text-white md:px-6 md:py-3.5 md:text-base bg-accent-500",
          className,
        )}
      >
        {children}
      </a>
    </span>
  );
}
