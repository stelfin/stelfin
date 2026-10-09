import type { ComponentProps } from "react";
import { CtaIcon } from "@/components/ui/cta-icon";
import type { Cta } from "@/lib/data/site";
import { cn } from "@/lib/utils/cn";

// The other platform, beside the primary button.
//
// Outlined and still, where the primary is filled and magnetic. Two filled
// buttons side by side are two primaries, and a visitor asked to choose
// between equals reads both and presses neither. This one is plainly second:
// the same shape and height, so the row is a row, and none of the weight.
export function SecondaryCta({ cta, className, ...props }: { cta: Cta } & Omit<ComponentProps<"a">, "href">) {
  return (
    <a
      {...props}
      href={cta.href}
      target="_blank"
      rel="noopener"
      className={cn(
        "flex items-center gap-2.5 rounded-control border border-line-strong bg-bg-raised px-3 py-2 text-sm font-medium text-fg transition-[transform,border-color,color] duration-300 hover:-translate-y-0.5 hover:border-accent hover:text-accent active:translate-y-0 md:px-6 md:py-3.5 md:text-base",
        className,
      )}
    >
      <CtaIcon kind={cta.kind} />
      <span>{cta.label}</span>
    </a>
  );
}
