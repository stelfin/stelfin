"use client";

import { useEffect, useRef, useState } from "react";

/**
 * CustomCursor
 *
 * A small dot that follows the cursor and grows when over interactive
 * elements. Spring-damped chasing rather than direct positioning so the dot
 * trails the actual mouse with a tiny lag. Disabled on touch devices.
 */
export function CustomCursor() {
  const ref = useRef<HTMLDivElement | null>(null);
  const [isGrown, setIsGrown] = useState(false);

  useEffect(() => {
    if (window.matchMedia("(pointer: coarse)").matches) return;

    document.documentElement.classList.add("has-custom-cursor");

    let targetX = window.innerWidth / 2;
    let targetY = window.innerHeight / 2;
    let currentX = targetX;
    let currentY = targetY;
    const LERP = 0.18;

    function onMove(e: MouseEvent) {
      targetX = e.clientX;
      targetY = e.clientY;

      const el = e.target as HTMLElement;
      const isInteractive =
        el.closest("a, button, [data-cursor='grow'], input, textarea, select") !== null;
      setIsGrown(isInteractive);
    }

    let raf = 0;
    function tick() {
      currentX += (targetX - currentX) * LERP;
      currentY += (targetY - currentY) * LERP;
      if (ref.current) {
        ref.current.style.transform = `translate(${currentX}px, ${currentY}px) translate(-50%, -50%)`;
      }
      raf = requestAnimationFrame(tick);
    }
    raf = requestAnimationFrame(tick);
    window.addEventListener("mousemove", onMove);

    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener("mousemove", onMove);
      document.documentElement.classList.remove("has-custom-cursor");
    };
  }, []);

  return (
    <div
      ref={ref}
      aria-hidden="true"
      className={`pointer-events-none fixed left-0 top-0 z-[100] transition-[width,height,background-color,border-color] duration-200 ease-out ${
        isGrown
          ? "h-10 w-10 rounded-full border border-accent-500 bg-transparent mix-blend-difference"
          : "h-3 w-3 rounded-full bg-accent-500 shadow-[0_0_0_3px_rgba(20,113,61,0.18)]"
      }`}
    />
  );
}
