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

  // Two places a key can be, because a key has two states and they must not
  // be confused.
  //
  // KEY holds the key of a wallet that exists. PENDING holds one that was
  // generated here for a wallet that has not been created yet. There used to
  // be only the first, written the moment the page opened, and a page that
  // found anything in it refused to go on — so a link that expired, a request
  // that failed, or a tab closed before pressing the button left a key behind
  // for a wallet nobody had, and every later link on this device stopped at
  // "this device already has a wallet", with nothing anywhere to clear it.
  const KEY = "stelfin.key";
  const PENDING = "stelfin.key.pending";

  function fail(title, detail) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    box.appendChild(document.createTextNode(detail));
    // Announced, and focused. This box is the most important thing on the
    // page and nothing was telling a screen reader it had appeared, so a
    // refusal was silent to anyone not looking at it.
    box.setAttribute("role", "alert");
    box.hidden = false;
    box.focus();
    $("pending").hidden = true;
    $("done").hidden = true;
  }

  // Same reasoning as confirm.js: the token lives in the fragment so it never
  // reaches the server as part of a URL, and stripping it from the address
  // bar means a screenshot or shoulder-surf of the URL does not carry
  // enrollment authority.
  const token = StelfinLink.token();

  if (!token) {
    fail("This link is incomplete.", "Open the link from your chat message again.");
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
    const tx = StellarSdk.TransactionBuilder.fromXDR(xdr, networkPassphrase);
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
    // The SDK prints an amount with seven decimal places, so nothing is
    // "0.0000000" and never "0". Comparing against the bare digit refused every
    // provisioning transaction this server has ever built. Matched as text and
    // not as a number: a starting balance is exactly zero or it is refused, and
    // no float gets a say in which.
    if (!/^0(\.0+)?$/.test(String(create.startingBalance))) {
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
    // A key for a wallet that exists means this device already has one.
    // Overwriting it silently would orphan the account it belongs to — the
    // person needs to know, not have it replaced underneath them.
    if (localStorage.getItem(KEY)) {
      fail("This device already has a wallet.",
        "It stays as it is. To replace it, hand it back first: send /reclaim to stelfin in your chat, and this device can then be given a new one.");
      return;
    }

    // A key from an attempt that did not finish is the one to carry on with.
    // The address somebody was shown last time is the address they are shown
    // now, and whatever stopped them — the link running out, the connection,
    // the tab — costs them nothing but asking for another link.
    let keypair = null;
    try {
      const unfinished = localStorage.getItem(PENDING);
      if (unfinished) keypair = StellarSdk.Keypair.fromSecret(unfinished);
    } catch (_) {
      keypair = null;
    }
    if (!keypair) {
      keypair = StellarSdk.Keypair.random();
      // Persisted immediately, before any network call: if the tab closes or
      // the connection drops mid-flow, the key that was shown is the key that
      // exists, rather than one that only ever lived in memory.
      localStorage.setItem(PENDING, keypair.secret());
    }

    // The wallet exists: its key moves to where the other pages look for it.
    const keep = () => {
      localStorage.setItem(KEY, keypair.secret());
      localStorage.removeItem(PENDING);
    };

    // The one moment a copy of the key can be taken. It is shown only when
    // asked for, because the usual place to open this page is a phone held
    // where other people can see it; and it is offered at all because a key
    // that exists in one browser's storage and nowhere else is a wallet that
    // clearing that browser's data destroys. stelfin cannot help afterwards.
    // It never had the key, which is the point of all of this.
    $("reveal").addEventListener("click", () => {
      $("secret").textContent = keypair.secret();
      $("backup").hidden = false;
      $("reveal").hidden = true;
      $("reveal").setAttribute("aria-expanded", "true");
      $("secret").focus();
    });
    $("copySecret").addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(keypair.secret());
        $("copied").hidden = false;
      } catch (_) {
        // No clipboard here — an in-app browser, or permission refused. The
        // key is on screen and selects whole when tapped.
        $("copied").textContent = "Copying is not available in this browser. Select the key above and copy it by hand.";
        $("copied").hidden = false;
      }
    });

    $("address").textContent = keypair.publicKey();
    // Held closed until there is something behind it. The button is on screen
    // from here, and the transaction it signs arrives a round trip later — a
    // tap in between used to land on a button with no handler and do nothing,
    // with nothing to say so.
    $("create").disabled = true;
    $("pending").hidden = false;

    let built;
    try {
      built = await api("/v1/enroll", { address: keypair.publicKey() });
    } catch (err) {
      if (err.status === 409) {
        fail("You already have a wallet.",
          "Go back to your chat and send — if it was set up on this device, its key is still here. If it was set up on another device, that is the one that signs.");
      } else if (err.status === 401) {
        fail("This link has expired.", "Message stelfin again to get a new one. The address above will be the same.");
      } else if (err.status === 403) {
        fail("This workspace is not setting wallets up.",
          "An admin there can turn it on. Nothing was created.");
      } else if (err.status === 425) {
        fail("It is a little too soon.",
          "stelfin waits a few minutes after first meeting someone before setting a wallet up. Nothing was created — message it again shortly.");
      } else if (err.status === 429) {
        fail("No more wallets can be set up right now.",
          "A limit for today has been reached. Nothing was created — try again tomorrow, or link a wallet you already have.");
      } else {
        fail("Something went wrong.", "Nothing was created. Message stelfin again for a new link.");
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

    $("create").disabled = false;
    $("create").addEventListener("click", async () => {
      const button = $("create");
      busy(button, true);
      button.textContent = "Creating…";
      try {
        verified.tx.sign(keypair);
        await api("/v1/enroll/submit", { signed_xdr: verified.tx.toXDR() });
        keep();
        $("pending").hidden = true;
        $("done").hidden = false;
      } catch (err) {
        button.disabled = false;
        button.textContent = "Create wallet";
        if (err.status === 409) {
          // Already submitted: this very transaction went through before,
          // and only the answer was lost. The wallet exists and this is its
          // key.
          keep();
          fail("This wallet has already been created.", "You can close this page and go back to your chat.");
        } else if (err.status === 410) {
          fail("This request expired before it was sent.", "Message stelfin again to start over.");
        } else {
          fail("The wallet could not be created.", "Please try again in a moment.");
        }
      }
    });
  })();
})();
