export interface FaqItem {
  id: string;
  question: string;
  answer: string;
}

export const FAQS: FaqItem[] = [
  {
    id: "what-is-stelfin",
    question: "What is stelfin?",
    answer:
      "A non-custodial stablecoin wallet you use entirely through WhatsApp. Message it what you want to send, confirm on your device, and it settles on Stellar.",
  },
  {
    id: "download-app",
    question: "Do I need to download an app?",
    answer:
      "No. stelfin runs inside WhatsApp, the app you already use. The only browser moment is signing a payment or setting up your wallet — both open from a link stelfin sends you, and close again once you're done.",
  },
  {
    id: "custodial",
    question: "Can stelfin move my money without me?",
    answer:
      "No. Your signing key is generated on your own device and never transmitted anywhere, including to us. stelfin can build a transaction; only you can authorize it.",
  },
  {
    id: "lost-device",
    question: "What if I lose my device?",
    answer:
      "Account recovery isn't built yet — this is an early testnet build. Right now, losing the device that holds your key means losing access to that wallet. Don't send anything you can't afford to lose while this is testnet.",
  },
  {
    id: "real-money",
    question: "Is this ready for real money?",
    answer:
      "Not yet. This is a public testnet demo — testnet Stellar, no real value moving. We'll say so clearly, right here, when that changes.",
  },
  {
    id: "get-started",
    question: "How do I get started?",
    answer:
      'Tap any "Chat on WhatsApp" button on this page. stelfin will ask you to set up a wallet first — a couple of taps — and then you\'re ready to send.',
  },
];
