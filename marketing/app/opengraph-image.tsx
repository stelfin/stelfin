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
          <div
            style={{
              width: 64,
              height: 64,
              borderRadius: 18,
              background: "#14713D",
              display: "flex",
            }}
          />
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
