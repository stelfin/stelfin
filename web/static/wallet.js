// Signing with a wallet the person already has, without the transaction ever
// leaving their sight.
//
// The pages here could always be signed for by a wallet they do not hold the
// key to: the envelope is shown, copied out, signed somewhere, and pasted back.
// That works with every wallet there is and is a lot to ask of somebody
// approving a payment. This is the same exchange with the copying taken out of
// it. The page hands the envelope to Freighter, Freighter shows it to the
// person and signs if they agree, and the page takes the signature back.
//
// Two things about that are deliberate.
//
// The page still decides what gets signed. Everything a page does to check an
// envelope — deriving what it does, comparing that with what the server
// claimed, refusing what its rules do not allow — happens before a button here
// exists. The wallet is asked to sign what the page has already agreed to show.
//
// And the page does not take the wallet's word for what it signed. What comes
// back is supposed to be the same transaction with one more signature. It is
// held to that: the same hash, exactly one signature the page did not already
// have, and that signature valid for the account the wallet says signed. Then
// the page adds that one signature to its own copy of the envelope and sends
// its own copy. Whatever else was in what the wallet returned is never sent
// anywhere. A wallet, or something standing where a wallet should be, that
// answered with a different transaction would be answering a page that is not
// listening.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory;
  } else {
    root.StelfinWallet = factory(root.StellarSdk, root.freighterApi, root.StelfinChain);
  }
})(typeof self !== "undefined" ? self : this, function (StellarSdk, freighter, chain) {
  "use strict";

  // Refused is anything that stops a signature being used, in words for the
  // person. Nothing has been sent when one is thrown.
  function Refused(title, detail) {
    this.name = "Refused";
    this.title = title;
    this.message = detail;
  }
  Refused.prototype = Object.create(Error.prototype);

  // The extension answers "are you there" within a couple of seconds or not at
  // all. Everything else it is asked waits on a person, so nothing else is
  // timed — but nothing else is asked until this has been answered yes, because
  // without the extension those questions are never answered at all and the
  // page would wait on them for good.
  const PRESENCE_TIMEOUT = 3000;
  const NETWORK_TIMEOUT = 8000;

  function within(promise, ms) {
    return Promise.race([
      promise,
      new Promise((_, reject) => setTimeout(() => reject(new Error("timeout")), ms)),
    ]);
  }

  // present reports whether Freighter is installed in this browser.
  async function present() {
    if (!freighter || typeof freighter.isConnected !== "function") return false;
    try {
      const answer = await within(freighter.isConnected(), PRESENCE_TIMEOUT);
      // Exactly true, and not merely something. The library decides the
      // extension is there by looking for a global called "freighter", and a
      // browser makes a global of every element's id — so a page with an
      // element named that would be told the extension was installed, by its
      // own markup, and would offer a button that waits for ever on a wallet
      // that is not there. A page here once had such an element. An element
      // is truthy and is not true.
      return Boolean(answer) && answer.isConnected === true;
    } catch (_) {
      return false;
    }
  }

  const networkName = (passphrase) => (chain ? chain.name(passphrase) : String(passphrase || ""));
  const short = (address) =>
    address && address.length > 12 ? address.slice(0, 6) + "…" + address.slice(-6) : String(address || "");

  // sign has Freighter sign xdr and returns the page's own envelope with that
  // one signature added: { xdr, signer }.
  //
  // onto is the envelope the signature is added to, when it is not xdr itself:
  // the same transaction already carrying other people's signatures. address,
  // when given, is the one account whose signature will do.
  async function sign({ xdr, onto, passphrase, address }) {
    if (!(await present())) {
      throw new Refused("Freighter is not in this browser.",
        "Nothing was signed. Sign in your own wallet below instead.");
    }

    const access = await freighter.requestAccess();
    const active = access && (access.address || access.publicKey);
    if (!access || access.error || !active) {
      throw new Refused("Freighter did not share an account.",
        "Nothing was signed. Unlock Freighter, allow this site, and try again.");
    }

    // Before the wallet is shown anything to sign. A signature is made over
    // the network's passphrase, so a wallet on another network would either
    // refuse or sign something that means nothing here, and the person should
    // hear which network to switch to rather than see a signature fail.
    let network;
    try {
      network = await within(freighter.getNetworkDetails(), NETWORK_TIMEOUT);
    } catch (_) {
      throw new Refused("Freighter did not say which network it is on.",
        "Nothing was signed. Open Freighter, check it is unlocked, and try again.");
    }
    if (!network || network.error || network.networkPassphrase !== passphrase) {
      const theirs = network && (networkName(network.networkPassphrase) || network.network);
      throw new Refused("Freighter is on a different network.",
        "This is for " + networkName(passphrase) + (theirs ? ", and Freighter is set to " + theirs : "") +
        ". Switch networks in Freighter and try again. Nothing was signed.");
    }

    const answer = await freighter.signTransaction(xdr, {
      networkPassphrase: passphrase,
      address: address || active,
    });
    if (!answer || answer.error || !answer.signedTxXdr) {
      throw new Refused("Nothing was signed.",
        "Freighter did not sign: it was declined there, or that account cannot sign this. Nothing was sent, and this page is as it was.");
    }
    return addOne(onto || xdr, answer.signedTxXdr, passphrase, answer.signerAddress || address || active, address);
  }

  // addOne takes the one new signature out of what a wallet returned and puts
  // it on base, the page's own envelope. It refuses anything that is not base
  // plus exactly one valid signature from signer.
  function addOne(base, returned, passphrase, signer, wanted) {
    let ours, theirs;
    try {
      ours = StellarSdk.TransactionBuilder.fromXDR(base, passphrase);
    } catch (_) {
      throw new Refused("This page lost track of what it was signing.", "Nothing was sent. Reload the page and try again.");
    }
    try {
      theirs = StellarSdk.TransactionBuilder.fromXDR(returned, passphrase);
    } catch (_) {
      throw new Refused("The wallet's answer could not be read.", "Nothing was sent.");
    }

    // A transaction's hash covers everything except its signatures. The same
    // hash is the transaction shown on this page, whatever else is true.
    if (!ours.hash().equals(theirs.hash())) {
      throw new Refused("That is not this transaction.",
        "The wallet signed something other than what is shown here. Nothing was sent.");
    }
    if (wanted && signer !== wanted) {
      throw new Refused("That is a different account.",
        "This needs the signature of " + short(wanted) + ", and the wallet signed as " + short(signer) +
        ". Switch accounts in the wallet and try again. Nothing was sent.");
    }

    let key;
    try {
      key = StellarSdk.Keypair.fromPublicKey(signer);
    } catch (_) {
      throw new Refused("The wallet did not say who signed.", "Nothing was sent.");
    }

    const had = new Set(ours.signatures.map((s) => s.signature().toString("base64")));
    const fresh = theirs.signatures.filter((s) => !had.has(s.signature().toString("base64")));
    if (fresh.length === 0) {
      throw new Refused("That account has already signed this.",
        "Its signature is on the envelope already. Another signer has to add theirs.");
    }
    const digest = ours.hash();
    const valid = fresh.filter((s) => key.verify(digest, s.signature()));
    if (fresh.length !== 1 || valid.length !== 1) {
      throw new Refused("The wallet's answer could not be used.",
        "It should have added exactly one signature, from the account it named. Nothing was sent.");
    }

    ours.signatures.push(valid[0]);
    return { xdr: ours.toXDR(), signer: signer };
  }

  // offer puts a sign-with-Freighter button on a page, if Freighter is there.
  //
  //   section   the element to reveal when Freighter is present
  //   button    the button inside it
  //   envelope  () => { xdr, onto?, passphrase, address? }, read at the click
  //   signed    async ({ xdr, signer }) => …, what to do with the result
  //   refused   (title, detail) => …, how this page shows a correctable error
  //   absent    optional element to reveal instead when Freighter is missing
  async function offer({ section, button, envelope, signed, refused, absent }) {
    if (!section || !button) return false;
    if (!(await present())) {
      if (absent) absent.hidden = false;
      return false;
    }
    section.hidden = false;
    button.addEventListener("click", async () => {
      button.disabled = true;
      button.setAttribute("aria-busy", "true");
      try {
        await signed(await sign(envelope()));
      } catch (err) {
        if (err instanceof Refused) {
          refused(err.title, err.message);
        } else {
          refused("Signing did not finish.", "Nothing was sent. Try again, or sign in your own wallet below.");
        }
      } finally {
        button.disabled = false;
        button.setAttribute("aria-busy", "false");
      }
    });
    return true;
  }

  return { present, sign, addOne, offer, Refused };
});
