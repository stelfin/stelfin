import { ImageResponse } from "next/og";

export const alt = "stelfin: run your DAO treasury from Telegram and Discord";
export const size = { width: 1200, height: 630 };
export const contentType = "image/png";

// Generated rather than a checked-in PNG.
//
// Every share of this site was previously a blank card: there was no OG image
// at all and no metadataBase for one to resolve against. A typographic card
// built here stays in sync with the copy, renders crisply at any scale, and
// costs no binary in the repo. It is drawn in the brand's own colours rather
// than photography because the site's visual language is diagrammatic, and a
// stock photograph would be the only photograph anywhere near the product.
export default function Image() {
  return new ImageResponse(
    (
      <div
        style={{
          height: "100%",
          width: "100%",
          display: "flex",
          flexDirection: "column",
          justifyContent: "space-between",
          background: "#0e120f",
          padding: "80px",
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: "20px" }}>
          {/* The mark itself, the same artwork as app/icon.svg. This was a
              blank green square: the shape of the logo with the logo left
              out, on the one image a shared link is judged by. */}
          <svg width={64} height={64} viewBox="0 0 32 32">
            <rect width="32" height="32" rx="9" fill="#14713D" />
            <path
              d="M8 10.5A2.5 2.5 0 0 1 10.5 8h11A2.5 2.5 0 0 1 24 10.5v8a2.5 2.5 0 0 1-2.5 2.5H14l-4.5 3.5V21h-1A2.5 2.5 0 0 1 6 18.5v-8Z"
              fill="none"
              stroke="#ffffff"
              strokeWidth="1.8"
              strokeLinejoin="round"
            />
            <circle cx="16" cy="14.2" r="3" fill="#ffffff" />
            <path
              d="M16 12.6v3.2M15.1 13.4h1.8M15.1 15h1.5"
              stroke="#14713D"
              strokeWidth="0.9"
              strokeLinecap="round"
            />
          </svg>
          <div style={{ fontSize: 44, fontWeight: 700, color: "#fafaf9", letterSpacing: "-0.02em" }}>
            stelfin
          </div>
        </div>

        <div
          style={{
            display: "flex",
            fontSize: 76,
            lineHeight: 1.1,
            letterSpacing: "-0.03em",
            color: "#fafaf9",
            maxWidth: 940,
          }}
        >
          Run your DAO treasury from the chat you already use
        </div>

        <div style={{ display: "flex", fontSize: 30, color: "#7dd6a0" }}>
          Non-custodial Stellar operations, in Telegram and Discord
        </div>
      </div>
    ),
    { ...size },
  );
}
