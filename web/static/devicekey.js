// The key this device holds for a wallet stelfin set up, if it holds one.
//
// One module because there were three copies of this, on three pages, and they
// had to agree with a fourth page that wrote what they read. They did not quite:
// the page that makes a key stored it before the wallet existed, and nothing
// that read it could tell a wallet's key from the leftover of an attempt that
// never finished.
//
// So a key is in one of two places. KEY holds the key of a wallet that exists.
// PENDING holds one made for a wallet that had not been created when it was
// written. The enroll page moves a key from the second to the first when it
// learns the wallet exists — and it does not always learn. The request that
// creates a wallet can succeed and its answer be lost, to a closed tab or a
// dropped connection, and then the wallet is on the network with its key still
// filed under "unfinished".
//
// Which is why nothing here asks "is there a key". It asks "is there a key for
// this address", of both places. A page that needs a signature for a wallet
// names the wallet; if the key for it turns up among the unfinished, then the
// wallet evidently exists, because the server has just described a transaction
// from it, and the key is moved to where it belongs.
//
// Asking by address is also the safety property the pages already relied on.
// Having a key is not enough to sign with it: a key for some other wallet would
// sign without complaint, and the first sign of trouble would be the network
// refusing a transaction the page had called sent.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory;
  } else {
    root.StelfinDeviceKey = factory(root.StellarSdk, root.localStorage);
  }
})(typeof self !== "undefined" ? self : this, function (StellarSdk, storage) {
  "use strict";

  const KEY = "stelfin.key";
  const PENDING = "stelfin.key.pending";

  function read(slot) {
    let seed = null;
    try {
      seed = storage.getItem(slot);
    } catch (_) {
      // Storage refused: a private window, or a browser that blocks it. There
      // is no key to be had, which is an ordinary way to hold a wallet.
      return null;
    }
    if (!seed) return null;
    try {
      return StellarSdk.Keypair.fromSecret(seed);
    } catch (_) {
      return null;
    }
  }

  // keyFor returns the keypair this device holds for address, or null.
  function keyFor(address) {
    if (!address) return null;

    const kept = read(KEY);
    if (kept && kept.publicKey() === address) return kept;

    const unfinished = read(PENDING);
    if (unfinished && unfinished.publicKey() === address) {
      // Only into an empty place. A different wallet's key is already there
      // if it is not empty, and nothing here is worth overwriting a key for.
      if (!kept) {
        try {
          storage.setItem(KEY, unfinished.secret());
          storage.removeItem(PENDING);
        } catch (_) {
          // Could not be moved. It still signs, which is what was asked for.
        }
      }
      return unfinished;
    }
    return null;
  }

  // forget removes the key for address, wherever it is, and no other.
  //
  // For an account that has been handed back and no longer exists. Its key
  // opens nothing, and left in place it is what makes the enroll page say this
  // device already has a wallet.
  function forget(address) {
    if (!address) return;
    for (const slot of [KEY, PENDING]) {
      const held = read(slot);
      if (held && held.publicKey() === address) {
        try {
          storage.removeItem(slot);
        } catch (_) {
          // Nothing to be done, and nothing depends on it having worked.
        }
      }
    }
  }

  return { keyFor, forget, KEY, PENDING };
});
