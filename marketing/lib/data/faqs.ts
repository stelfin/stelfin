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
      "A non-custodial bot your DAO runs its Stellar treasury through, from Telegram or Discord. Ask it to pay someone, approve on your own device, and it settles on Stellar.",
  },
  {
    id: "download-app",
    question: "Do I need to download an app?",
    answer:
      "No. stelfin lives in the group chat your community already uses. The only browser moment is signing a payment or linking your wallet — both open from a link stelfin sends you privately, and close again once you're done.",
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
      "The Telegram and Discord bots are being built in the open right now — the source link on this page is the honest answer until they are ready. Nothing here is live yet.",
  },
];
