import { Navbar } from "@/components/layout/navbar";
import { Footer } from "@/components/layout/footer";
import { Hero } from "@/components/sections/hero";
import { TrustBand } from "@/components/sections/trust-band";
import { HowItWorksSection } from "@/components/sections/how-it-works-section";
import { ProposalsSection } from "@/components/sections/proposals-section";
import { SecuritySection } from "@/components/sections/security-section";
import { ConnectorsSection } from "@/components/sections/connectors-section";
import { FaqSection } from "@/components/sections/faq-section";
import { ClosingCta } from "@/components/sections/closing-cta";
import { TryItSection } from "@/components/sections/try-it-section";
import { LIVE } from "@/lib/data/site";

export default function HomePage() {
  return (
    <>
      <Navbar />
      <main>
        <Hero />
        <TrustBand />
        <HowItWorksSection />
        <ProposalsSection />
        <SecuritySection />
        <ConnectorsSection />
        {/* Only once there is a bot to try. See lib/data/site.ts. */}
        {LIVE && <TryItSection />}
        <FaqSection />
        <ClosingCta />
      </main>
      <Footer />
    </>
  );
}
