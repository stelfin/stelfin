// What to call a network, and where to look a transaction up on it.
//
// The pages are told which network they are on by its passphrase, because the
// passphrase is what a signature is made over and so is the thing that has to
// be exactly right. It is not what a person calls it. "Test SDF Network ;
// September 2015" printed beside an address reads as a fault, to somebody whose
// one question is whether this is real money.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.StelfinChain = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  const NETWORKS = {
    "Test SDF Network ; September 2015": {
      name: "Stellar testnet (no real money)",
      explorer: "https://stellar.expert/explorer/testnet",
    },
    "Public Global Stellar Network ; September 2015": {
      name: "Stellar public network",
      explorer: "https://stellar.expert/explorer/public",
    },
  };

  // name is what to show for a passphrase. One this page has never heard of is
  // shown as it is: an unfamiliar network is exactly what somebody about to
  // sign should be looking at, not something to paper over.
  function name(passphrase) {
    const known = NETWORKS[passphrase];
    return known ? known.name : String(passphrase || "");
  }

  // txURL is where a transaction can be looked up, or "" when there is nowhere
  // known to send somebody.
  //
  // The hash is checked to be a hash before it goes into a URL. It comes from
  // the server, and what is being built is a link a person will follow.
  function txURL(passphrase, hash) {
    const known = NETWORKS[passphrase];
    if (!known || !/^[0-9a-f]{64}$/.test(String(hash || ""))) return "";
    return known.explorer + "/tx/" + hash;
  }

  // point makes anchor a link to the transaction, and shows it; or leaves it
  // hidden when there is nowhere to point.
  //
  // noopener and noreferrer, and a new tab. The page somebody leaves is one
  // they may still need, and nothing about it should travel with them.
  function point(anchor, passphrase, hash) {
    const url = txURL(passphrase, hash);
    if (!anchor || !url) return false;
    anchor.href = url;
    anchor.target = "_blank";
    anchor.rel = "noopener noreferrer";
    anchor.hidden = false;
    return true;
  }

  return { name, txURL, point };
});
