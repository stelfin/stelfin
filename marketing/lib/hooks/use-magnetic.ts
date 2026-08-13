"use client";

import { useEffect, useRef } from "react";

interface UseMagneticOptions {
  /** Maximum pixel distance the element travels toward the cursor. */
  strength?: number;
  /** Radius (px) within which the magnetic effect activates. */
  radius?: number;
}

/**
 * useMagnetic
 *
 * Returns a ref to attach to any element you want to "pull" toward the
 * cursor when nearby. Bails on touch devices since there's no cursor to
 * follow.
 */
export function useMagnetic<T extends HTMLElement>({
  strength = 24,
  radius = 120,
}: UseMagneticOptions = {}) {
  const ref = useRef<T | null>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (window.matchMedia("(pointer: coarse)").matches) return;

    let targetX = 0;
    let targetY = 0;
    let currentX = 0;
    let currentY = 0;
    let raf = 0;

    function onMove(e: MouseEvent) {
      if (!el) return;
      const rect = el.getBoundingClientRect();
      const cx = rect.left + rect.width / 2;
      const cy = rect.top + rect.height / 2;
      const dx = e.clientX - cx;
      const dy = e.clientY - cy;
      const distance = Math.hypot(dx, dy);

      if (distance < radius) {
        const factor = (1 - distance / radius) * (strength / radius);
        targetX = dx * factor;
        targetY = dy * factor;
      } else {
        targetX = 0;
        targetY = 0;
      }
    }

    function onLeave() {
      targetX = 0;
      targetY = 0;
    }

    function tick() {
      currentX += (targetX - currentX) * 0.15;
      currentY += (targetY - currentY) * 0.15;
      if (el) {
        el.style.transform = `translate(${currentX}px, ${currentY}px)`;
      }
      raf = requestAnimationFrame(tick);
    }

    raf = requestAnimationFrame(tick);
    window.addEventListener("mousemove", onMove);
    el.addEventListener("mouseleave", onLeave);

    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener("mousemove", onMove);
      el.removeEventListener("mouseleave", onLeave);
    };
  }, [strength, radius]);

  return ref;
}
