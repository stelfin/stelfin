"use client";

import { motion, useInView, type Variants } from "framer-motion";
import { useRef, type ReactNode } from "react";

interface RevealProps {
  children: ReactNode;
  delay?: number;
  from?: "bottom" | "left" | "right" | "fade";
  className?: string;
  amount?: number;
  disabled?: boolean;
  /** Replay every time the element crosses the viewport boundary. Default true. */
  repeat?: boolean;
}

/**
 * Reveal
 *
 * Generic wrapper that fades + slides its children into view as they cross
 * the viewport boundary. The defaults are intentionally subtle — 24px of
 * travel, 0.7s duration — heavier movement undercuts a quiet tone.
 */
export function Reveal({
  children,
  delay = 0,
  from = "bottom",
  className,
  amount = 0.2,
  disabled = false,
  repeat = true,
}: RevealProps) {
  const ref = useRef<HTMLDivElement | null>(null);
  const isInView = useInView(ref, {
    once: !repeat,
    amount,
    margin: "-10% 0px -10% 0px",
  });

  const variants: Variants = {
    hidden: {
      opacity: 0,
      y: from === "bottom" ? 24 : 0,
      x: from === "left" ? -24 : from === "right" ? 24 : 0,
      transition: { duration: 0.5, ease: [0.16, 1, 0.3, 1] },
    },
    visible: {
      opacity: 1,
      y: 0,
      x: 0,
      transition: { duration: 0.7, delay, ease: [0.16, 1, 0.3, 1] },
    },
  };

  if (disabled) {
    return <div className={className}>{children}</div>;
  }

  return (
    <motion.div
      ref={ref}
      initial="hidden"
      animate={isInView ? "visible" : "hidden"}
      variants={variants}
      className={className}
    >
      {children}
    </motion.div>
  );
}
