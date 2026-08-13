import { Navbar } from "@/components/layout/navbar";
import { Footer } from "@/components/layout/footer";
import { Hero } from "@/components/sections/hero";
import { HowItWorksSection } from "@/components/sections/how-it-works-section";
import { SecuritySection } from "@/components/sections/security-section";
import { FaqSection } from "@/components/sections/faq-section";
import { ClosingCta } from "@/components/sections/closing-cta";

export default function HomePage() {
  return (
    <>
      <Navbar />
      <main>
        <Hero />
        <HowItWorksSection />
        <SecuritySection />
        <FaqSection />
        <ClosingCta />
      </main>
      <Footer />
    </>
  );
}
