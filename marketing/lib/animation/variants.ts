import type { Variants } from "framer-motion";

/**
 * Shared motion language for the whole site.
 *
 * One easing curve, one set of entrance variants — so every section animates
 * with the same "voice" rather than each component inventing its own timing.
 */

// easeOutExpo — slow, confident finish.
export const EASE = [0.16, 1, 0.3, 1] as const;

/** Fade + rise into view. `delay` lets callers choreograph a sequence. */
export const fadeUp = (delay = 0, y = 18): Variants => ({
  hidden: { opacity: 0, y },
  visible: {
    opacity: 1,
    y: 0,
    transition: { duration: 0.7, delay, ease: EASE },
  },
});

/** Parent that releases its children one after another. */
export const stagger = (staggerChildren = 0.08, delayChildren = 0): Variants => ({
  hidden: {},
  visible: { transition: { staggerChildren, delayChildren } },
});

/** A single word/line rising from behind a clip mask. */
export const maskWord: Variants = {
  hidden: { y: "115%", transition: { duration: 0.5, ease: EASE } },
  visible: { y: 0, transition: { duration: 0.8, ease: EASE } },
};
