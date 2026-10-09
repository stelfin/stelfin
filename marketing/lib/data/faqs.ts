import { BOT, LIVE } from "@/lib/data/site";

export interface FaqItem {
  id: string;
  question: string;
  answer: string;
}

// The last answer on the page, in its two states. Which one is shown follows
// from whether the site has been told where the bot is — the same switch that
// decides what the buttons do, so the two cannot disagree. See lib/data/site.ts.
const WHERE =
  BOT.telegram && BOT.discord
    ? "your Telegram group or your Discord server"
    : BOT.telegram
      ? "your Telegram group"
      : "your Discord server";

const GET_STARTED = LIVE
  ? `Add stelfin to ${WHERE} with the button on this page, and have an admin of the group run /setup there. Each person who will sign then runs /link to prove a wallet is theirs. It runs on Stellar's test network, so nothing you move is real money: the steps under "Try it on testnet" get you a wallet and test funds.`
  : "The Telegram and Discord bots are being built in the open right now, and the source link on this page is the honest answer until they are ready. Nothing here is live yet.";

export const FAQS: FaqItem[] = [
  {
    id: "what-is-stelfin",
    question: "What is stelfin?",
    answer:
      "A non-custodial bot your DAO runs its Stellar treasury through, from Telegram or Discord. A member proposes a payment in the group chat, the treasury's signers approve it on their own devices, and it settles on Stellar.",
  },
  {
    id: "download-app",
    question: "Do I need to download an app?",
    answer:
      "No. stelfin lives in the group chat your community already uses. The only browser moment is approving a transaction or linking your wallet. Both open from a link stelfin sends you privately, and close again once you are done.",
  },
  {
    id: "approvals",
    question: "How do approvals work when the treasury has several signers?",
    answer:
      "The way your treasury already works. stelfin reads the signing weights off the account and collects approvals until the transaction has the weight it needs. Nothing is submitted before that threshold, and anyone can see who has signed and who has not.",
  },
  {
    id: "contract-treasury",
    question: "Does the treasury have to be a multisig account?",
    answer:
      "No. stelfin can also create a treasury that is a contract on Stellar, which its members vote in. Each member votes from their own wallet; the contract counts the votes, holds the money, and makes a passed proposal wait before it is paid, so there is time to stop one. Nobody holds a key over it: not whoever created it, and not us.",
  },
  {
    id: "custodial",
    question: "Can stelfin move the treasury without us?",
    answer:
      "No. A signer's key stays in their own wallet, on their own device, and is never sent anywhere, including to us. stelfin can build a transaction and ask; only the signers can authorize it.",
  },
  {
    id: "connectors",
    question: "Can a connector or integration spend the treasury?",
    answer:
      "No. A connector can read a spreadsheet and propose a batch of payments, and that is the whole of what it can do. The proposal still goes to the treasury's signers, and a human still approves it before anything moves.",
  },
  {
    id: "lost-device",
    question: "What if a signer loses their device?",
    answer:
      "In a multisig treasury, one signer losing a device is not the treasury losing access. The remaining signers can still reach the threshold, and the lost key can be removed from the account. A single-signer treasury has no such margin, which is a reason to run more than one.",
  },
  {
    id: "real-money",
    question: "Is this ready for real money?",
    answer:
      "Not yet. This is a public testnet demo, so no real value is moving. We will say so clearly, right here, when that changes.",
  },
  {
    id: "get-started",
    question: "How do I get started?",
    answer: GET_STARTED,
  },
];
