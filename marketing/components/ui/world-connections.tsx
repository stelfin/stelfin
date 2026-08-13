// A dot-matrix world map with a handful of animated arcs radiating from
// Lagos — the "message becomes a payment, anywhere" visual the hero needed
// once the phone mockup came out. Deterministic (no Math.random) so server
// and client render identically; the motion itself is native SVG
// <animateMotion>, not JS, so this needs no "use client" and ships no extra
// script.

interface Point {
  x: number;
  y: number;
}

// Rough continent silhouettes as simple polygons — stylised, not surveyed.
// This is a faint background texture, not an atlas.
const CONTINENTS: Point[][] = [
  // North America
  [
    { x: 70, y: 60 }, { x: 140, y: 40 }, { x: 220, y: 50 }, { x: 270, y: 90 },
    { x: 260, y: 140 }, { x: 230, y: 180 }, { x: 200, y: 230 }, { x: 170, y: 260 },
    { x: 140, y: 240 }, { x: 110, y: 200 }, { x: 80, y: 150 }, { x: 60, y: 100 },
  ],
  // South America
  [
    { x: 200, y: 270 }, { x: 260, y: 260 }, { x: 300, y: 300 }, { x: 310, y: 360 },
    { x: 290, y: 420 }, { x: 260, y: 460 }, { x: 230, y: 440 }, { x: 210, y: 380 }, { x: 195, y: 320 },
  ],
  // Europe
  [
    { x: 440, y: 50 }, { x: 500, y: 40 }, { x: 560, y: 60 }, { x: 550, y: 110 },
    { x: 500, y: 150 }, { x: 460, y: 140 }, { x: 440, y: 100 },
  ],
  // Africa
  [
    { x: 440, y: 170 }, { x: 500, y: 160 }, { x: 560, y: 180 }, { x: 580, y: 250 },
    { x: 560, y: 330 }, { x: 520, y: 400 }, { x: 480, y: 420 }, { x: 450, y: 380 },
    { x: 440, y: 300 }, { x: 430, y: 220 },
  ],
  // Asia
  [
    { x: 560, y: 60 }, { x: 650, y: 20 }, { x: 750, y: 30 }, { x: 850, y: 60 },
    { x: 920, y: 100 }, { x: 900, y: 150 }, { x: 850, y: 180 }, { x: 800, y: 220 },
    { x: 750, y: 260 }, { x: 700, y: 280 }, { x: 650, y: 240 }, { x: 600, y: 180 }, { x: 570, y: 120 },
  ],
  // Australia
  [
    { x: 800, y: 340 }, { x: 870, y: 330 }, { x: 920, y: 350 }, { x: 910, y: 400 },
    { x: 860, y: 420 }, { x: 810, y: 400 }, { x: 790, y: 370 },
  ],
];

// Ray-casting point-in-polygon test.
function inside(pt: Point, poly: Point[]): boolean {
  let isIn = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const a = poly[i]!;
    const b = poly[j]!;
    const intersects = a.y > pt.y !== b.y > pt.y && pt.x < ((b.x - a.x) * (pt.y - a.y)) / (b.y - a.y) + a.x;
    if (intersects) isIn = !isIn;
  }
  return isIn;
}

function buildDots(): Point[] {
  const dots: Point[] = [];
  const step = 15;
  for (let x = 0; x <= 1000; x += step) {
    for (let y = 0; y <= 500; y += step) {
      const p = { x, y };
      if (CONTINENTS.some((poly) => inside(p, poly))) dots.push(p);
    }
  }
  return dots;
}

const DOTS = buildDots();

// Lagos as the hub every arc radiates from — the product's actual target
// market — reaching four points spanning the kind of distance "anywhere"
// implies: London, New York, Dubai, Singapore.
const LAGOS: Point = { x: 468, y: 272 };
const DESTINATIONS: { point: Point; duration: string; delay: string }[] = [
  { point: { x: 478, y: 92 }, duration: "3.2s", delay: "0s" }, // London
  { point: { x: 182, y: 152 }, duration: "4s", delay: "0.8s" }, // New York
  { point: { x: 622, y: 192 }, duration: "2.6s", delay: "1.6s" }, // Dubai
  { point: { x: 782, y: 262 }, duration: "3.6s", delay: "2.4s" }, // Singapore
];

// Quadratic control point lifted above the midpoint — reads as a great-circle
// arc rather than a straight line.
function arcPath(a: Point, b: Point): string {
  const mx = (a.x + b.x) / 2;
  const my = (a.y + b.y) / 2;
  const lift = Math.hypot(b.x - a.x, b.y - a.y) * 0.28;
  return `M ${a.x} ${a.y} Q ${mx} ${my - lift} ${b.x} ${b.y}`;
}

export function WorldConnections() {
  return (
    <svg
      viewBox="0 0 1000 500"
      className="pointer-events-none absolute inset-0 h-full w-full"
      preserveAspectRatio="xMidYMid slice"
      aria-hidden="true"
    >
      {DOTS.map((d, i) => (
        <circle key={i} cx={d.x} cy={d.y} r={1.6} className="fill-ink-900/[0.09]" />
      ))}

      {DESTINATIONS.map(({ point, duration, delay }, i) => {
        const path = arcPath(LAGOS, point);
        return (
          <g key={i}>
            <path d={path} fill="none" stroke="currentColor" strokeWidth={1} className="text-accent-500/25" />
            <circle r={2.6} className="fill-accent-500">
              <animateMotion dur={duration} begin={delay} repeatCount="indefinite" path={path} />
            </circle>
            <circle cx={point.x} cy={point.y} r={2.5} className="fill-accent-500/70" />
          </g>
        );
      })}

      {/* Lagos — the origin every message becomes a payment from. */}
      <circle cx={LAGOS.x} cy={LAGOS.y} r={4} className="fill-accent-500" />
      <circle cx={LAGOS.x} cy={LAGOS.y} r={4} className="fill-accent-500/40">
        <animate attributeName="r" values="4;16;4" dur="2.4s" repeatCount="indefinite" />
        <animate attributeName="opacity" values="0.5;0;0.5" dur="2.4s" repeatCount="indefinite" />
      </circle>
    </svg>
  );
}
