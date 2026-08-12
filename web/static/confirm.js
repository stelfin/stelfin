(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  function fail(title, detail) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    // textContent, never innerHTML: some of this text originates with the
    // user's own message and must never be interpreted as markup.
    box.appendChild(document.createTextNode(detail));
    box.hidden = false;
    $("payment").hidden = true;
  }

  // The token lives in the fragment so it never reaches the server as part of
  // a URL, and so it stays out of access logs and Referer headers. Strip it
  // from the address bar immediately: a screenshot or shoulder-surf of the URL
  // should not carry payment authority.
  const token = location.hash.replace(/^#/, "");
  history.replaceState(null, "", location.pathname);

  if (!token) {
    fail("This link is incomplete.", "Open the link from your WhatsApp message again.");
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
    const tx = new StellarSdk.TransactionBuilder.fromXDR(xdr, networkPassphrase);
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

  function loadSigningKey() {
    const seed = localStorage.getItem("stelfin.key");
    if (!seed) return null;
    try {
      return StellarSdk.Keypair.fromSecret(seed);
    } catch {
      return null;
    }
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

    const key = loadSigningKey();
    if (!key) {
      $("confirm").disabled = true;
      fail("This device isn't set up yet.",
        "Your wallet key is not on this device, so this payment cannot be signed here. " +
        "Open the link on the device you set up, or start enrolment again.");
      $("payment").hidden = false;
      return;
    }

    $("confirm").addEventListener("click", async () => {
      const button = $("confirm");
      button.disabled = true;
      button.textContent = "Sending…";
      try {
        envelope.tx.sign(key);
        const result = await api("/v1/submit", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ signed_xdr: envelope.tx.toXDR() }),
        });
        button.textContent = "Sent";
        $("payment").querySelector(".note").textContent =
          "Sent. Reference " + short(result.hash) + ". You can close this page.";
      } catch (err) {
        button.disabled = false;
        button.textContent = "Send";
        if (err.status === 409) {
          fail("This payment has already been sent.", "Nothing was sent twice.");
        } else if (err.status === 410) {
          fail("This payment expired before it was sent.",
            "Nothing has been sent. Send your message again to start over.");
        } else {
          fail("The payment could not be sent.",
            "Nothing has been sent. Please try again in a moment.");
        }
      }
    });
  })();
})();
