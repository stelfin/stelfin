(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const KEY = "stelfin.key";

  function fail(title, detail) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    box.appendChild(document.createTextNode(detail));
    box.hidden = false;
    $("pending").hidden = true;
    $("done").hidden = true;
  }

  // Same reasoning as confirm.js: the token lives in the fragment so it never
  // reaches the server as part of a URL, and stripping it from the address
  // bar means a screenshot or shoulder-surf of the URL does not carry
  // enrollment authority.
  const token = location.hash.replace(/^#/, "");
  history.replaceState(null, "", location.pathname);

  if (!token) {
    fail("This link is incomplete.", "Open the link from your WhatsApp message again.");
    return;
  }

  async function api(path, body) {
    const res = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const text = (await res.text()).trim();
      const err = new Error(text || res.statusText);
      err.status = res.status;
      throw err;
    }
    return res.json();
  }

  // Re-derive what this transaction actually does, the same posture as
  // confirm.js's readEnvelope: the server's job was to build it, not to be
  // trusted about what it built. Refuses anything that is not exactly the
  // four sponsored-creation operations, sourced the way this device expects —
  // treasury sourcing the sponsorship and creation, this device's own fresh
  // address sourcing the trustline and the end of sponsorship.
  function verifyProvisioning(xdr, networkPassphrase, expectedAddress) {
    const tx = new StellarSdk.TransactionBuilder.fromXDR(xdr, networkPassphrase);
    const ops = tx.operations;
    if (ops.length !== 4) {
      throw new Error("this transaction does not look like account creation");
    }
    const [begin, create, trust, end] = ops;
    if (begin.type !== "beginSponsoringFutureReserves" || begin.sponsoredId !== expectedAddress) {
      throw new Error("sponsorship is not for this device's address");
    }
    if (create.type !== "createAccount" || create.destination !== expectedAddress) {
      throw new Error("this transaction does not create this device's address");
    }
    if (create.startingBalance !== "0") {
      throw new Error("this transaction funds the account with unexpected XLM");
    }
    if (trust.type !== "changeTrust" || trust.source !== expectedAddress) {
      throw new Error("the trustline is not sourced from this device's address");
    }
    if (end.type !== "endSponsoringFutureReserves" || end.source !== expectedAddress) {
      throw new Error("sponsorship is not ended by this device's address");
    }
    return { tx, assetCode: trust.line.getCode ? trust.line.getCode() : "USDC" };
  }

  (async () => {
    // An existing key means this device already has a wallet. Overwriting it
    // silently would orphan whatever account it belongs to — the user needs
    // to know, not have it replaced underneath them.
    if (localStorage.getItem(KEY)) {
      fail("This device already has a wallet.",
        "If you meant to start over, clear this device's saved key first — otherwise your existing wallet stays as it is.");
      return;
    }

    const keypair = StellarSdk.Keypair.random();
    // Persisted immediately, before any network call: if the tab closes or
    // the connection drops mid-flow, the key that was shown is the key that
    // exists, rather than one that only ever lived in memory.
    localStorage.setItem(KEY, keypair.secret());

    $("address").textContent = keypair.publicKey();
    $("pending").hidden = false;

    let built;
    try {
      built = await api("/v1/enroll", { address: keypair.publicKey() });
    } catch (err) {
      if (err.status === 409) {
        fail("This number already has a wallet.", "Message stelfin again — you should be able to send already.");
      } else if (err.status === 401) {
        fail("This link has expired.", "Message stelfin again to get a new one.");
      } else {
        fail("Something went wrong.", "Nothing was created. Please try again in a moment.");
      }
      return;
    }

    let verified;
    try {
      verified = verifyProvisioning(built.xdr, built.network_passphrase, keypair.publicKey());
    } catch (err) {
      fail("This request could not be verified.", "Nothing was created. " + err.message);
      return;
    }

    $("create").addEventListener("click", async () => {
      const button = $("create");
      button.disabled = true;
      button.textContent = "Creating…";
      try {
        verified.tx.sign(keypair);
        await api("/v1/enroll/submit", { signed_xdr: verified.tx.toXDR() });
        $("pending").hidden = true;
        $("done").hidden = false;
      } catch (err) {
        button.disabled = false;
        button.textContent = "Create wallet";
        if (err.status === 409) {
          fail("This wallet has already been created.", "You can close this page and go back to WhatsApp.");
        } else if (err.status === 410) {
          fail("This request expired before it was sent.", "Message stelfin again to start over.");
        } else {
          fail("The wallet could not be created.", "Please try again in a moment.");
        }
      }
    });
  })();
})();
