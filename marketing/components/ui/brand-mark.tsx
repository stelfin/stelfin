export function BrandMark() {
  return (
    <div className="flex items-center gap-2">
      <svg viewBox="0 0 32 32" className="h-7 w-7 md:h-8 md:w-8" aria-hidden="true">
        <rect width="32" height="32" rx="9" fill="#14713D" />
        <path
          d="M10 20.5c0 2 1.8 3 4.2 3 2.6 0 4-1 4-2.6 0-1.8-1.7-2.3-3.8-2.8-2.4-.5-4.2-1.1-4.2-3.1 0-1.7 1.6-2.8 4-2.8 2.2 0 3.9 1 4.1 2.9"
          stroke="white"
          strokeWidth="2"
          strokeLinecap="round"
          fill="none"
        />
      </svg>
      <span className="text-[22px] font-bold leading-none tracking-tight text-ink-900 md:text-[24px]">
        stelfin
      </span>
    </div>
  );
}
