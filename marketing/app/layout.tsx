import type { Metadata, Viewport } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import { MotionConfig } from "framer-motion";
import "@/app/globals.css";
import { SmoothScroll } from "@/components/interactive/smooth-scroll";

const geist = Geist({ subsets: ["latin"], variable: "--font-sans", display: "swap" });

const geistMono = Geist_Mono({ subsets: ["latin"], variable: "--font-mono", display: "swap" });

const title = "stelfin: run your DAO treasury from Telegram and Discord";
const description =
  "Propose a payment in the group chat. Signers approve it on their own devices. No signer's key ever leaves their phone, and stelfin never holds one.";

// Needed for the OG and Twitter image URLs to resolve to absolute addresses.
// Without it Next emits relative paths, which every social scraper drops, and
// the card renders blank no matter how good the image is.
export const metadata: Metadata = {
  metadataBase: new URL("https://stelfin.vercel.app"),
  title,
  description,
  openGraph: { title, description, type: "website", siteName: "stelfin" },
  twitter: { card: "summary_large_image", title, description },
};

// Matches the page background in each mode, so the browser chrome on mobile
// does not sit as a bright strip above a dark page.
export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#fafaf9" },
    { media: "(prefers-color-scheme: dark)", color: "#0e120f" },
  ],
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${geist.variable} ${geistMono.variable}`}>
      <body className="relative min-h-screen overflow-x-hidden">
        <MotionConfig reducedMotion="user">
          <SmoothScroll>{children}</SmoothScroll>
        </MotionConfig>
      </body>
    </html>
  );
}
