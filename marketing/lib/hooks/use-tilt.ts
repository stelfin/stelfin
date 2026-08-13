"use client";

import { useMotionValue, useSpring, useReducedMotion } from "framer-motion";
import type React from "react";

interface UseTiltOptions {
  /** Max degrees of rotation at the edges. */
  max?: number;
  stiffness?: number;
  damping?: number;
  mass?: number;
}

/**
 * useTilt
 *
 * Spring-damped 3D tilt toward the cursor. Returns motion values to bind to
 * rotateX/rotateY plus the mouse handlers. Bails on reduced-motion.
 */
export function useTilt({
  max = 7,
  stiffness = 120,
  damping = 18,
  mass = 0.4,
}: UseTiltOptions = {}) {
  const reduce = useReducedMotion();
  const rx = useMotionValue(0);
  const ry = useMotionValue(0);
  const rotateX = useSpring(rx, { stiffness, damping, mass });
  const rotateY = useSpring(ry, { stiffness, damping, mass });

  function onMouseMove(e: React.MouseEvent<HTMLElement>) {
    if (reduce) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const nx = (e.clientX - rect.left) / rect.width - 0.5;
    const ny = (e.clientY - rect.top) / rect.height - 0.5;
    rx.set(-ny * max);
    ry.set(nx * max);
  }

  function onMouseLeave() {
    rx.set(0);
    ry.set(0);
  }

  return { rotateX, rotateY, onMouseMove, onMouseLeave };
}
