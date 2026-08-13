"use client";

import { motion, useInView, type Variants } from "framer-motion";
import { useRef, type ElementType } from "react";
import { maskWord } from "@/lib/animation/variants";
import { cn } from "@/lib/utils/cn";

interface MaskRevealProps {
  text: string;
  /** A substring of `text` to render in the accent color. */
  accent?: string;
  as?: "h1" | "h2" | "h3" | "p" | "span";
  className?: string;
  delay?: number;
  stagger?: number;
  /** "inView" reveals on scroll; "mount" reveals immediately (hero). */
  trigger?: "inView" | "mount";
  amount?: number;
  repeat?: boolean;
}

/**
 * MaskReveal
 *
 * Words rise from behind a clip mask, one after another. The site's
 * signature heading treatment — used on the hero and every section title.
 */
export function MaskReveal({
  text,
  accent,
  as = "h2",
  className,
  delay = 0,
  stagger = 0.07,
  trigger = "inView",
  amount = 0.4,
  repeat = true,
}: MaskRevealProps) {
  const ref = useRef<HTMLElement | null>(null);
  const inView = useInView(ref, {
    once: trigger === "mount" ? true : !repeat,
    amount,
    margin: "-10% 0px -10% 0px",
  });
  const animate = trigger === "mount" || inView ? "visible" : "hidden";

  const container: Variants = {
    hidden: {},
    visible: { transition: { staggerChildren: stagger, delayChildren: delay } },
  };

  let accentStart = -1;
  let accentEnd = -1;
  if (accent) {
    const idx = text.indexOf(accent);
    if (idx >= 0) {
      accentStart = idx;
      accentEnd = idx + accent.length;
    }
  }

  const words: { w: string; accent: boolean }[] = [];
  let cursor = 0;
  for (const w of text.split(" ")) {
    const start = cursor;
    const end = cursor + w.length;
    cursor = end + 1;
    words.push({
      w,
      accent: accentStart >= 0 && start >= accentStart && end <= accentEnd,
    });
  }

  const MotionTag = motion[as] as ElementType;

  return (
    <MotionTag
      ref={ref}
      variants={container}
      initial="hidden"
      animate={animate}
      className={cn("flex flex-wrap gap-x-[0.28em] gap-y-1", className)}
    >
      {words.map((item, i) => (
        <span key={i} className="inline-block overflow-hidden pb-[0.12em]">
          <motion.span
            variants={maskWord}
            className={cn("inline-block", item.accent && "text-accent-500")}
          >
            {item.w}
          </motion.span>
        </span>
      ))}
    </MotionTag>
  );
}
