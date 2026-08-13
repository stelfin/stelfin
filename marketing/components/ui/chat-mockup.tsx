import type { ReactNode } from "react";
import { motion, type HTMLMotionProps } from "framer-motion";
import { cn } from "@/lib/utils/cn";

/**
 * Shared WhatsApp chat-mockup building blocks — the header chrome, message
 * area, bubbles, and confirmation card used across the hero's looping chat.
 */

interface ChatHeaderProps {
  subtitle?: string;
}

export function ChatHeader({ subtitle = "Online" }: ChatHeaderProps) {
  return (
    <div className="flex items-center gap-2 bg-[#1F2C34] px-2.5 pb-2.5 pt-12 text-white">
      <button type="button" aria-label="Back" className="flex items-center gap-0.5 text-white">
        <svg viewBox="0 0 24 24" className="h-5 w-5">
          <path d="M14 6l-6 6 6 6" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" fill="none" />
        </svg>
        <span className="text-[13px] font-medium">1</span>
      </button>

      <div className="grid h-8 w-8 shrink-0 place-items-center overflow-hidden rounded-full bg-white">
        <svg viewBox="0 0 32 32" className="h-5 w-5" aria-hidden="true">
          <rect width="32" height="32" rx="9" fill="#14713D" />
          <path
            d="M10 20.5c0 2 1.8 3 4.2 3 2.6 0 4-1 4-2.6 0-1.8-1.7-2.3-3.8-2.8-2.4-.5-4.2-1.1-4.2-3.1 0-1.7 1.6-2.8 4-2.8 2.2 0 3.9 1 4.1 2.9"
            stroke="white"
            strokeWidth="2.2"
            strokeLinecap="round"
            fill="none"
          />
        </svg>
      </div>

      <div className="min-w-0 flex-1 leading-tight">
        <div className="flex items-center gap-1">
          <p className="text-[14px] font-semibold">stelfin</p>
          <svg viewBox="0 0 24 24" className="h-3.5 w-3.5 shrink-0">
            <circle cx="12" cy="12" r="10" fill="#22C55E" />
            <path d="M8 12.2l2.6 2.6 5-5" stroke="white" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" fill="none" />
          </svg>
        </div>
        <p className="text-[10px] text-white/60">{subtitle}</p>
      </div>

      <button type="button" aria-label="Video call" className="grid h-9 w-9 shrink-0 place-items-center text-white">
        <svg viewBox="0 0 24 24" className="h-5 w-5">
          <rect x="2" y="6" width="14" height="12" rx="2" stroke="currentColor" strokeWidth="2" fill="none" />
          <path d="M22 7v10l-6-4v-2l6-4z" fill="currentColor" />
        </svg>
      </button>
      <button type="button" aria-label="Voice call" className="grid h-9 w-9 shrink-0 place-items-center text-white">
        <svg viewBox="0 0 24 24" className="h-5 w-5">
          <path
            d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6A19.79 19.79 0 0 1 2.12 4.18 2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72c.13.96.37 1.9.7 2.81a2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45c.91.33 1.85.57 2.81.7A2 2 0 0 1 22 16.92z"
            stroke="currentColor"
            strokeWidth="2"
            fill="none"
            strokeLinejoin="round"
            strokeLinecap="round"
          />
        </svg>
      </button>
    </div>
  );
}

interface ChatScreenProps {
  children: ReactNode;
  subtitle?: string;
  align?: "start" | "end";
}

export function ChatScreen({ children, subtitle, align = "start" }: ChatScreenProps) {
  return (
    <div className="flex h-full flex-col">
      <ChatHeader subtitle={subtitle} />
      <div className={cn("bg-chat flex flex-1 flex-col gap-2 overflow-hidden p-3", align === "end" && "justify-end")}>
        {children}
      </div>
    </div>
  );
}

interface BubbleProps {
  side: "in" | "out";
  time: string;
  children: ReactNode;
}

export function StaticBubble({ side, time, children }: BubbleProps) {
  const isOut = side === "out";
  return (
    <div className={`flex ${isOut ? "justify-end" : "justify-start"}`}>
      <div
        className={`max-w-[80%] whitespace-pre-line rounded-2xl px-3 py-2 text-[11px] leading-snug ${
          isOut ? "rounded-br-md bg-accent-500 text-white" : "rounded-bl-md bg-white text-ink-900"
        }`}
      >
        <div>{children}</div>
        <div className={`mt-0.5 text-right text-[9px] ${isOut ? "text-white/60" : "text-ink-400"}`}>
          {time}
          {isOut && <span className="ml-1">✓✓</span>}
        </div>
      </div>
    </div>
  );
}

export function AnimatedBubble({ side, time, children, ...motionProps }: BubbleProps & HTMLMotionProps<"div">) {
  const isOut = side === "out";
  return (
    <motion.div {...motionProps} className={`flex ${isOut ? "justify-end" : "justify-start"}`}>
      <div
        className={`max-w-[80%] whitespace-pre-line rounded-2xl px-3 py-2 text-[11px] leading-snug ${
          isOut ? "rounded-br-md bg-accent-500 text-white" : "rounded-bl-md bg-white text-ink-900"
        }`}
      >
        <div>{children}</div>
        <div className={`mt-0.5 text-right text-[9px] ${isOut ? "text-white/60" : "text-ink-400"}`}>
          {time}
          {isOut && <span className="ml-1">✓✓</span>}
        </div>
      </div>
    </motion.div>
  );
}

interface ReceiptCardProps {
  status: string;
  statusTone: "confirmed" | "pending";
  amount: string;
  detail: string;
  reference?: string;
  time?: string;
  className?: string;
}

const STATUS_BADGE: Record<ReceiptCardProps["statusTone"], { label: string; className: string }> = {
  confirmed: { label: "✓ Sent", className: "bg-emerald-50 text-emerald-700" },
  pending: { label: "Awaiting signature", className: "bg-accent-50 text-accent-600" },
};

/** The single source of truth for the "confirm this payment" card shown inside the hero's chat mockup. */
export function ReceiptCard({ status, statusTone, amount, detail, reference, time, className }: ReceiptCardProps) {
  const badge = STATUS_BADGE[statusTone];

  return (
    <div className={cn("w-[85%] rounded-2xl rounded-bl-md bg-white p-3 shadow-sm ring-1 ring-ink-200/40", className)}>
      <div className="flex items-center justify-between">
        <span className="text-[10px] font-medium uppercase tracking-wider text-ink-500">{status}</span>
        <span className={cn("rounded-full px-2 py-0.5 text-[9px] font-medium", badge.className)}>{badge.label}</span>
      </div>
      <p className="mt-1.5 font-sans text-2xl font-semibold leading-none tabular-nums text-ink-900">{amount}</p>
      <p className="mt-1 text-[11px] text-ink-500">{detail}</p>
      {(reference || time) && (
        <p className="mt-2 font-mono text-[9px] text-ink-300">
          {reference}
          {reference && time ? " · " : ""}
          {time}
        </p>
      )}
    </div>
  );
}
