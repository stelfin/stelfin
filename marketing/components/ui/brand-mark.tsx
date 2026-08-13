// A message bubble carrying a coin — literally what the product does,
// rather than an abstract monogram. Reused at the size the header/footer
// need it at; the glyph itself is drawn once and just scales.
export function BrandMark() {
  return (
    <div className="flex items-center gap-2">
      <svg viewBox="0 0 32 32" className="h-7 w-7 md:h-8 md:w-8" aria-hidden="true">
        <rect width="32" height="32" rx="9" fill="#14713D" />
        <path
          d="M8 10.5A2.5 2.5 0 0 1 10.5 8h11A2.5 2.5 0 0 1 24 10.5v8a2.5 2.5 0 0 1-2.5 2.5H14l-4.5 3.5V21h-1A2.5 2.5 0 0 1 6 18.5v-8Z"
          fill="none"
          stroke="white"
          strokeWidth="1.8"
          strokeLinejoin="round"
        />
        <circle cx="16" cy="14.2" r="3" fill="white" />
        <path d="M16 12.6v3.2M15.1 13.4h1.8M15.1 15h1.5" stroke="#14713D" strokeWidth="0.9" strokeLinecap="round" />
      </svg>
      <span className="text-[22px] font-bold leading-none tracking-tight text-ink-900 md:text-[24px]">stelfin</span>
    </div>
  );
}
