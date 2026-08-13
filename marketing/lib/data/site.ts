// Placeholder until Meta's WhatsApp Business API is live — swap the number
// once STELFIN_META_PHONE_NUMBER_ID stops being a placeholder in the Go
// deployment too. See STELFIN_BASE_URL in the backend's own env for the
// same "not real yet" caveat.
const WHATSAPP_NUMBER = "000000000000";

export const SITE = {
  name: "stelfin",
  legalName: "stelfin",
  whatsappLink: `https://wa.me/${WHATSAPP_NUMBER}?text=${encodeURIComponent("hi")}`,
  githubUrl: "https://github.com/stelfin/stelfin",
  appUrl: "https://stelfin.onrender.com",
};

export const NAV_LINKS = [
  { href: "#how-it-works", label: "How it works" },
  { href: "#security", label: "Security" },
  { href: "#faq", label: "FAQ" },
];
