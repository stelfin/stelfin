import type { Metadata } from "next";
import { Inter, Instrument_Serif, JetBrains_Mono } from "next/font/google";
import { MotionConfig } from "framer-motion";
import "@/app/globals.css";
import { SmoothScroll } from "@/components/interactive/smooth-scroll";
import { CustomCursor } from "@/components/interactive/custom-cursor";

const inter = Inter({ subsets: ["latin"], variable: "--font-sans", display: "swap" });

const instrumentSerif = Instrument_Serif({
  subsets: ["latin"],
  weight: "400",
  style: ["normal", "italic"],
  variable: "--font-display",
  display: "swap",
});

const jetbrainsMono = JetBrains_Mono({ subsets: ["latin"], variable: "--font-mono", display: "swap" });

const title = "stelfin — send stablecoins on Stellar, right from WhatsApp";
const description =
  "stelfin is a non-custodial stablecoin wallet you talk to instead of open. No app, no seed phrase — your key stays on your device.";

export const metadata: Metadata = {
  title,
  description,
  openGraph: { title, description, type: "website", siteName: "stelfin" },
  twitter: { card: "summary", title, description },
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${inter.variable} ${instrumentSerif.variable} ${jetbrainsMono.variable}`}>
      <body className="relative min-h-screen overflow-x-hidden">
        <MotionConfig reducedMotion="user">
          <SmoothScroll>
            <CustomCursor />
            {children}
          </SmoothScroll>
        </MotionConfig>
      </body>
    </html>
  );
}
