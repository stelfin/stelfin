// A message bubble carrying a coin — literally what the product does,
// rather than an abstract monogram. Reused at the size the header/footer
// need it at; the glyph itself is drawn once and just scales.
//
// Two colour notes, both load-bearing:
//
// The fills are Tailwind classes rather than SVG presentation attributes,
// because a presentation attribute does not resolve var() — a hardcoded
// #14713D here is a hex the theme cannot reach, and it went stale in dark mode.
//
// The wordmark is text-current, not a fixed colour. It was text-ink-900 sitting
// on the footer's bg-ink-900: the brand name was rendering at 1:1 contrast,
// invisible, on every page load. Inheriting means the header (dark on light)
// and the footer (light on dark) both get it right from their own context.
export function BrandMark() {
  return (
    <div className="flex items-center gap-2">
      <svg viewBox="0 0 32 32" className="h-7 w-7 md:h-8 md:w-8" aria-hidden="true">
        <rect width="32" height="32" rx="9" className="fill-accent-500" />
        <path
          d="M8 10.5A2.5 2.5 0 0 1 10.5 8h11A2.5 2.5 0 0 1 24 10.5v8a2.5 2.5 0 0 1-2.5 2.5H14l-4.5 3.5V21h-1A2.5 2.5 0 0 1 6 18.5v-8Z"
          fill="none"
          stroke="white"
          strokeWidth="1.8"
          strokeLinejoin="round"
        />
        <circle cx="16" cy="14.2" r="3" fill="white" />
        <path
          d="M16 12.6v3.2M15.1 13.4h1.8M15.1 15h1.5"
          className="stroke-accent-500"
          strokeWidth="0.9"
          strokeLinecap="round"
        />
      </svg>
      <span className="text-[22px] font-bold leading-none tracking-tight text-current md:text-[24px]">
        stelfin
      </span>
    </div>
  );
}
