"use client";

import { motion, AnimatePresence } from "framer-motion";
import { useEffect, useState } from "react";
import { ChatHeader, AnimatedBubble, ReceiptCard } from "@/components/ui/chat-mockup";

type Turn =
  | { kind: "bubble"; side: "in" | "out"; text: string; time: string }
  | { kind: "typing"; side: "in" | "out" }
  | { kind: "receipt"; amount: string; destination: string; time: string };

// The bot's reply text here is the real format from api/inbound.go's
// HandleInbound — not invented marketing copy.
const SCRIPT: { turn: Turn; displayMs: number }[] = [
  { turn: { kind: "bubble", side: "out", text: "send 5,000 to brother", time: "9:14" }, displayMs: 1400 },
  { turn: { kind: "typing", side: "in" }, displayMs: 900 },
  {
    turn: {
      kind: "bubble",
      side: "in",
      text: 'Send 5,000.00 USDC to Brother?\n\nYou said: "5,000" to "brother"\n\nTap to confirm — the link expires in 10 minutes.',
      time: "9:14",
    },
    displayMs: 2800,
  },
  { turn: { kind: "typing", side: "out" }, displayMs: 900 },
  {
    turn: { kind: "receipt", amount: "5,000.00", destination: "Brother", time: "9:15" },
    displayMs: 3500,
  },
];

const LOOP_GAP_MS = 1200;

/**
 * LiveChatThread
 *
 * Auto-plays the scripted conversation on a loop, one setTimeout chain
 * driving turn-by-turn progression.
 */
export function LiveChatThread() {
  const [visibleTurns, setVisibleTurns] = useState<Turn[]>([]);

  useEffect(() => {
    let cancelled = false;
    const timeouts: ReturnType<typeof setTimeout>[] = [];

    function runScript() {
      if (cancelled) return;
      setVisibleTurns([]);

      let cumulativeDelay = 200;
      SCRIPT.forEach(({ turn, displayMs }, idx) => {
        const t = setTimeout(() => {
          if (cancelled) return;
          setVisibleTurns((prev) => {
            const last = prev[prev.length - 1];
            if (last?.kind === "typing" && turn.kind === "bubble" && last.side === turn.side) {
              return [...prev.slice(0, -1), turn];
            }
            if (last?.kind === "typing" && turn.kind === "receipt") {
              return [...prev.slice(0, -1), turn];
            }
            return [...prev, turn];
          });
          if (idx === SCRIPT.length - 1) {
            const restart = setTimeout(runScript, displayMs + LOOP_GAP_MS);
            timeouts.push(restart);
          }
        }, cumulativeDelay);
        timeouts.push(t);
        cumulativeDelay += displayMs;
      });
    }

    runScript();

    return () => {
      cancelled = true;
      timeouts.forEach(clearTimeout);
    };
  }, []);

  return (
    <div className="flex h-full flex-col">
      <ChatHeader />
      <div className="bg-chat flex flex-1 flex-col justify-end gap-2 overflow-hidden p-3">
        <AnimatePresence initial={false}>
          {visibleTurns.map((turn, idx) => (
            <TurnView key={`${idx}-${turn.kind}`} turn={turn} />
          ))}
        </AnimatePresence>
      </div>
    </div>
  );
}

function TurnView({ turn }: { turn: Turn }) {
  if (turn.kind === "typing") {
    return (
      <motion.div
        initial={{ opacity: 0, y: 6 }}
        animate={{ opacity: 1, y: 0 }}
        exit={{ opacity: 0 }}
        transition={{ duration: 0.25 }}
        className={`flex ${turn.side === "out" ? "justify-end" : "justify-start"}`}
      >
        <div className="flex items-center gap-1 rounded-2xl rounded-bl-md bg-white px-3 py-2.5 shadow-sm">
          {[0, 150, 300].map((delay) => (
            <span
              key={delay}
              className="inline-block h-1.5 w-1.5 rounded-full bg-ink-400"
              style={{ animation: `typing-dots 1.2s infinite ${delay}ms ease-in-out` }}
            />
          ))}
        </div>
      </motion.div>
    );
  }

  if (turn.kind === "receipt") {
    return (
      <motion.div
        initial={{ opacity: 0, y: 12, scale: 0.96 }}
        animate={{ opacity: 1, y: 0, scale: 1 }}
        exit={{ opacity: 0 }}
        transition={{ duration: 0.4, ease: [0.16, 1, 0.3, 1] }}
        className="flex justify-end"
      >
        <ReceiptCard
          status="Sent"
          statusTone="confirmed"
          amount={`${turn.amount} USDC`}
          detail={`to ${turn.destination}`}
          time={turn.time}
        />
      </motion.div>
    );
  }

  return (
    <AnimatedBubble
      side={turn.side}
      time={turn.time}
      initial={{ opacity: 0, y: 8 }}
      animate={{ opacity: 1, y: 0 }}
      exit={{ opacity: 0 }}
      transition={{ duration: 0.3, ease: [0.16, 1, 0.3, 1] }}
    >
      {turn.text}
    </AnimatedBubble>
  );
}
