(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  // A disabled button whose label changed to "Sending…" says "working" to
  // anyone looking at it and nothing at all to a screen reader. aria-busy is
  // the part that is actually announced. Only for work in flight: a button
  // held closed by a gate is not busy, it is simply not available yet.
  const busy = (el, on) => {
    el.disabled = on;
    el.setAttribute("aria-busy", String(on));
  };


  function fail(title, detail, keepPayment) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    // textContent, never innerHTML: some of this text originates with the
    // user's own message and must never be interpreted as markup.
    box.appendChild(document.createTextNode(detail));
    // Announced, and focused. This box is the most important thing on the
    // page and nothing was telling a screen reader it had appeared, so a
    // refusal was silent to anyone not looking at it.
    box.setAttribute("role", "alert");
    box.hidden = false;
    box.focus();
    // A refusal normally takes the payment off the screen: there is nothing
    // left to do with it. A paste that can simply be done again is different,
    // and hiding the box it goes in would make the mistake unfixable.
    $("payment").hidden = !keepPayment;
  }

  // The token lives in the fragment so it never reaches the server as part of
  // a URL, and so it stays out of access logs and Referer headers. Strip it
  // from the address bar immediately: a screenshot or shoulder-surf of the URL
  // should not carry payment authority.
  const token = StelfinLink.token();

  if (!token) {
    fail("This link is incomplete.", "Open the link from your chat message again.");
    return;
  }

  const short = (s) => (s && s.length > 12 ? s.slice(0, 6) + "…" + s.slice(-6) : s || "");

  async function api(path, options = {}) {
    const res = await fetch(path, {
      ...options,
      headers: { ...(options.headers || {}), Authorization: "Bearer " + token },
    });
    if (!res.ok) {
      const body = (await res.text()).trim();
      const err = new Error(body || res.statusText);
      err.status = res.status;
      throw err;
    }
    return res.json();
  }

  // Re-derive the payment from the envelope itself. This is the whole point of
  // the page: the server's summary is treated as a claim to be checked, not as
  // the thing to display.
  function readEnvelope(xdr, networkPassphrase) {
    const tx = StellarSdk.TransactionBuilder.fromXDR(xdr, networkPassphrase);
    if (tx.operations.length !== 1) {
      throw new Error("this transaction contains more than the payment shown");
    }
    const op = tx.operations[0];
    if (op.type !== "payment") {
      throw new Error("this transaction is not a payment");
    }
    return {
      amount: op.amount,
      destination: op.destination,
      assetCode: op.asset.isNative() ? "XLM" : op.asset.getCode(),
      tx,
    };
  }

  // Compare what the server said against what the envelope says. Amounts are
  // compared numerically so "5000" and "5,000.00" do not disagree over
  // formatting, while still catching any real difference in value.
  function agrees(serverAmountDisplay, envelopeAmount) {
    const stripped = String(serverAmountDisplay).replace(/,/g, "");
    return Number(stripped) === Number(envelopeAmount);
  }


  (async () => {
    let data;
    try {
      data = await api("/v1/confirm");
    } catch (err) {
      if (err.status === 404) {
        fail("This payment is no longer available.",
          "It may have already been sent, or the link may have expired. Send your message again to start over.");
      } else if (err.status === 401) {
        fail("This link has expired.", "Send your message again to get a new one.");
      } else {
        fail("Something went wrong.", "Nothing has been sent. Please try again in a moment.");
      }
      return;
    }

    let envelope;
    try {
      envelope = readEnvelope(data.xdr, data.network_passphrase);
    } catch (err) {
      fail("This payment could not be verified.",
        "Nothing has been sent. " + err.message);
      return;
    }

    // The check that makes the server untrusted for display purposes.
    if (!agrees(data.amount, envelope.amount) || data.to_address !== envelope.destination) {
      fail("This payment does not match what was described.",
        "Nothing has been sent. Please report this — the details shown to you differ from the " +
        "transaction itself.");
      return;
    }

    // Render from the envelope, falling back to the server's formatting only
    // for presentation of a value already confirmed to match.
    $("amount").textContent = data.amount;
    $("asset").textContent = envelope.assetCode;
    $("toLabel").textContent = data.to_label || short(envelope.destination);
    $("toShort").textContent = data.to_label ? "(" + short(envelope.destination) + ")" : "";
    $("toAddress").textContent = envelope.destination;
    $("hashShort").textContent = short(data.hash);
    $("said").textContent = data.said_amount
      ? `"${data.said_amount}" to "${data.said_destination}"`
      : "—";
    $("payment").hidden = false;

    $("fromAddress").textContent = data.from_address;

    // Sending, by whichever route produced the signature. One function, so the
    // two routes cannot come to report the same outcome differently.
    async function send(signedXDR, button, idle) {
      busy(button, true);
      button.textContent = "Sending…";
      try {
        const result = await api("/v1/submit", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ signed_xdr: signedXDR }),
        });
        button.textContent = "Sent";
        // Left disabled, and no longer busy: it is finished, not working.
        button.setAttribute("aria-busy", "false");
        $("alert").hidden = true;
        $("own").hidden = true;
        $("paymentNote").textContent =
          "Sent. Reference " + short(result.hash) + ". You can close this page.";
        StelfinChain.point($("explorer"), data.network_passphrase, result.hash);
      } catch (err) {
        busy(button, false);
        button.textContent = idle;
        if (err.status === 409) {
          fail("This payment has already been sent.", "Nothing was sent twice.");
        } else if (err.status === 410) {
          fail("This payment expired before it was sent.",
            "Nothing has been sent. Send your message again to start over.");
        } else if (err.status === 503) {
          fail("stelfin is not sending payments right now.",
            "Nothing has been sent. Please try again later.");
        } else {
          fail("The payment could not be sent.",
            "Nothing has been sent. Please try again in a moment.");
        }
      }
    }

    // The key on this device signs only if it is the key for the wallet this
    // payment comes from. Having *a* key is not enough: one for a different
    // wallet would sign without complaint, and the first sign of trouble would
    // be the network refusing the payment after the page had said "Sending".
    const key = StelfinDeviceKey.keyFor(data.from_address);
    if (key) {
      $("device").hidden = false;
      $("confirm").addEventListener("click", () => {
        envelope.tx.sign(key);
        send(envelope.tx.toXDR(), $("confirm"), "Send");
      });
      return;
    }

    // Otherwise the wallet is somewhere else — a hardware device, another app —
    // and that is an ordinary way to hold one, not an error. The page hands
    // over exactly the transaction it just verified and takes back the same
    // one, signed.
    $("envelope").value = data.xdr;
    StelfinClipboard.wire($("copyEnvelope"), $("envelope"), $("copiedEnvelope"));
    $("own").hidden = false;

    // A wallet in this browser can be asked directly. It has to sign as the
    // wallet this payment comes from: any other account's signature is one the
    // network will refuse, and saying so here is better than sending it.
    StelfinWallet.offer({
      section: $("walletOffer"),
      button: $("walletSign"),
      absent: $("noWallet"),
      envelope: () => ({ xdr: data.xdr, passphrase: data.network_passphrase, address: data.from_address }),
      signed: ({ xdr }) => send(xdr, $("walletSign"), "Sign with Freighter"),
      refused: (title, detail) => fail(title, detail, true),
    }).then((there) => {
      if (there) $("pasteHeading").textContent = "Or sign in another wallet";
    });
    const wanted = envelope.tx.hash().toString("hex");

    $("submitSigned").addEventListener("click", () => {
      const pasted = $("signed").value.trim();
      if (!pasted) {
        fail("Nothing to send.", "Paste the signed transaction first.", true);
        return;
      }
      let signed;
      try {
        signed = StellarSdk.TransactionBuilder.fromXDR(pasted, data.network_passphrase);
      } catch {
        fail("That is not a transaction.", "Paste the whole signed result from your wallet.", true);
        return;
      }
      // A transaction's hash covers everything except its signatures. The same
      // hash therefore means the same payment that is shown above, whatever
      // was done to it in between — and a different one means something else
      // was signed, which this page will not pass on.
      if (typeof signed.hash !== "function" || signed.hash().toString("hex") !== wanted) {
        fail("That is not this payment.",
          "What you pasted is a different transaction from the one shown here. Nothing has been sent.", true);
        return;
      }
      if (!signed.signatures.length) {
        fail("That transaction is not signed.", "Sign it in your wallet, then paste the result.", true);
        return;
      }
      send(pasted, $("submitSigned"), "Send signed payment");
    });
  })();
})();
