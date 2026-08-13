// A dot-matrix world map with animated arcs between several city pairs — "a
// message becomes a payment, anywhere" as a visual instead of a device
// screenshot. Deterministic (no Math.random) so server and client render
// identically; the motion itself is native SVG <animateMotion>, not JS, so
// this needs no "use client" and ships no extra script.

interface Point {
  x: number;
  y: number;
}

// Rough continent silhouettes as simple polygons — stylised, not surveyed.
// This is a background texture, not an atlas.
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

// Six points, none of them the sole origin — traffic moves between varied
// pairs, the way real corridors do, rather than fanning from one hub.
const CITIES = {
  lagos: { x: 468, y: 272 },
  london: { x: 478, y: 92 },
  newYork: { x: 182, y: 152 },
  dubai: { x: 622, y: 192 },
  singapore: { x: 782, y: 262 },
  nairobi: { x: 560, y: 300 },
};

const ARCS: { from: Point; to: Point; duration: string; delay: string }[] = [
  { from: CITIES.lagos, to: CITIES.london, duration: "3.2s", delay: "0s" },
  { from: CITIES.newYork, to: CITIES.london, duration: "3.8s", delay: "0.6s" },
  { from: CITIES.dubai, to: CITIES.singapore, duration: "2.8s", delay: "1.2s" },
  { from: CITIES.lagos, to: CITIES.dubai, duration: "3.4s", delay: "1.8s" },
  { from: CITIES.newYork, to: CITIES.lagos, duration: "4.2s", delay: "2.4s" },
  { from: CITIES.singapore, to: CITIES.nairobi, duration: "3s", delay: "3s" },
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
        <circle key={i} cx={d.x} cy={d.y} r={1.7} className="fill-ink-900/[0.22]" />
      ))}

      {ARCS.map(({ from, to, duration, delay }, i) => {
        const path = arcPath(from, to);
        return (
          <g key={i}>
            <path d={path} fill="none" stroke="currentColor" strokeWidth={1} className="text-accent-500/30" />
            <circle r={2.6} className="fill-accent-500">
              <animateMotion dur={duration} begin={delay} repeatCount="indefinite" path={path} />
            </circle>
          </g>
        );
      })}

      {Object.values(CITIES).map((c, i) => (
        <circle key={i} cx={c.x} cy={c.y} r={2.8} className="fill-accent-500/80" />
      ))}
    </svg>
  );
}
