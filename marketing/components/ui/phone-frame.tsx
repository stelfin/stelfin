"use client";

import { useEffect, useState, type ReactNode } from "react";
import { cn } from "@/lib/utils/cn";

interface PhoneFrameProps {
  children: ReactNode;
  className?: string;
}

export function PhoneFrame({ children, className }: PhoneFrameProps) {
  const [time, setTime] = useState("--:--");

  useEffect(() => {
    const updateTime = () => {
      setTime(
        new Date()
          .toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: true })
          .toLowerCase(),
      );
    };
    updateTime();
    const interval = setInterval(updateTime, 60_000);
    return () => clearInterval(interval);
  }, []);

  return (
    <div
      className={cn(
        "relative mx-auto aspect-[77.6/158] w-[300px] rounded-[3.2rem] p-[3px]",
        "[background:linear-gradient(135deg,#2e2e30_0%,#5d5d60_18%,#9c9c9f_38%,#cdcdd0_50%,#9c9c9f_62%,#5d5d60_82%,#2e2e30_100%)]",
        "shadow-[inset_0_0_0_0.5px_rgba(255,255,255,0.35),inset_0_1px_0_rgba(255,255,255,0.5),inset_0_-1px_0_rgba(0,0,0,0.5),0_40px_80px_-20px_rgba(10,10,10,0.55),0_0_0_1px_rgba(0,0,0,0.5),0_0_70px_-10px_rgba(190,195,200,0.45)]",
        className,
      )}
    >
      <div className="absolute -left-[3px] top-20 h-7 w-[3px] rounded-l-md [background:linear-gradient(90deg,#2e2e30_0%,#7a7a7d_60%,#3a3a3c_100%)] shadow-[inset_0_1px_0_rgba(255,255,255,0.3)]" />
      <div className="absolute -left-[3px] top-28 h-12 w-[3px] rounded-l-md [background:linear-gradient(90deg,#2e2e30_0%,#7a7a7d_60%,#3a3a3c_100%)] shadow-[inset_0_1px_0_rgba(255,255,255,0.3)]" />
      <div className="absolute -left-[3px] top-44 h-12 w-[3px] rounded-l-md [background:linear-gradient(90deg,#2e2e30_0%,#7a7a7d_60%,#3a3a3c_100%)] shadow-[inset_0_1px_0_rgba(255,255,255,0.3)]" />
      <div className="absolute -right-[3px] top-32 h-20 w-[3px] rounded-r-md [background:linear-gradient(270deg,#2e2e30_0%,#7a7a7d_60%,#3a3a3c_100%)] shadow-[inset_0_1px_0_rgba(255,255,255,0.3)]" />

      <div className="relative h-full w-full rounded-[3rem] bg-ink-900 p-[6px] shadow-[inset_0_0_0_1px_rgba(0,0,0,0.8)]">
        <div className="relative h-full w-full overflow-hidden rounded-[2.5rem] bg-surface-100">
          <div className="pointer-events-none absolute left-1/2 top-2 z-30 h-6 w-16 -translate-x-1/2 rounded-full bg-ink-900">
            <span className="absolute right-2.5 top-1/2 block h-1.5 w-1.5 -translate-y-1/2 rounded-full bg-ink-700" />
          </div>

          <div className="pointer-events-none absolute inset-x-0 top-2 z-20 flex h-6 items-center justify-between bg-[#1F2C34] px-4 text-[11px] font-semibold text-white">
            <span>{time}</span>
            <div className="flex items-center gap-[5px]">
              <CellularIcon />
              <WifiIcon />
              <BatteryIcon />
            </div>
          </div>

          <div className="h-full">{children}</div>

          <div className="pointer-events-none absolute inset-x-0 bottom-1.5 z-20 flex items-center justify-center">
            <span className="block h-[3px] w-20 rounded-full bg-white" />
          </div>
        </div>
      </div>
    </div>
  );
}

// Generic iOS-style status glyphs — plain UI chrome, not brand assets.
function CellularIcon() {
  return (
    <svg viewBox="0 0 18 12" className="h-[9px] w-auto" fill="white" aria-hidden="true">
      <rect x="0" y="7" width="3" height="5" rx="0.5" />
      <rect x="5" y="5" width="3" height="7" rx="0.5" />
      <rect x="10" y="3" width="3" height="9" rx="0.5" />
      <rect x="15" y="0" width="3" height="12" rx="0.5" />
    </svg>
  );
}
function WifiIcon() {
  return (
    <svg viewBox="0 0 16 12" className="h-[9px] w-auto" fill="none" aria-hidden="true">
      <path d="M1 4.5a10 10 0 0 1 14 0" stroke="white" strokeWidth="1.6" strokeLinecap="round" />
      <path d="M3.5 7.3a6.5 6.5 0 0 1 9 0" stroke="white" strokeWidth="1.6" strokeLinecap="round" />
      <circle cx="8" cy="10" r="1.3" fill="white" />
    </svg>
  );
}
function BatteryIcon() {
  return (
    <svg viewBox="0 0 25 12" className="h-[10px] w-auto" fill="none" aria-hidden="true">
      <rect x="0.5" y="0.5" width="21" height="11" rx="2.5" stroke="white" strokeOpacity="0.4" />
      <rect x="2" y="2" width="18" height="8" rx="1.5" fill="white" />
      <rect x="22.5" y="4" width="1.8" height="4" rx="0.9" fill="white" fillOpacity="0.4" />
    </svg>
  );
}
