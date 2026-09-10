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

  const KEY = "stelfin.key";

  const MEMBER = "link_member";
  const TREASURY = "link_treasury";

  // notice writes into the one alert box. `keepGoing` is what separates a dead
  // end from a step: a treasury envelope that has some signatures but not
  // enough is progress, and hiding the paste box under a red banner would tell
  // the next signer the attempt had failed.
  function notice(title, detail, keepGoing) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    box.appendChild(document.createTextNode(detail));
    box.classList.toggle("progress", Boolean(keepGoing));
    // Severity decides the role. A partially-signed envelope is progress,
    // and announcing it assertively would teach signers to tune out the
    // assertive announcement, which is the same reason this page does not
    // colour it red. Focus moves only on a real refusal.
    box.setAttribute("role", keepGoing ? "status" : "alert");
    box.hidden = false;
    if (!keepGoing) {
      box.focus();
      $("pending").hidden = true;
      $("done").hidden = true;
    }
  }

  function fail(title, detail) {
    notice(title, detail, false);
  }

  // Same reasoning as the other pages: the token lives in the fragment so it
  // never reaches the server as part of a URL, and stripping it from the
  // address bar means a screenshot or a shoulder-surf of the URL does not carry
  // the authority to complete someone's link.
  const token = location.hash.replace(/^#/, "");
  history.replaceState(null, "", location.pathname);

  if (!token) {
    fail("This link is incomplete.", "Open the link from your chat message again.");
    return;
  }

  async function api(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: {
        Authorization: "Bearer " + token,
        ...(body ? { "Content-Type": "application/json" } : {}),
      },
      body: body ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) {
      const text = (await res.text()).trim();
      const err = new Error(text || res.statusText);
      err.status = res.status;
      throw err;
    }
    return res.json();
  }

  // Re-derive what this transaction actually is, rather than trusting the
  // server's word for it. The server's job was to build the challenge; being
  // believed about what it built is a separate thing, and not one it gets.
  //
  // The decisive check is the sequence number. A SEP-10 challenge is built with
  // sequence 0, and an account's next usable sequence is always at least 1 — so
  // a sequence of 0 is proof the envelope can never be valid on chain, whoever
  // ends up holding it after signing. Anything else here is a transaction that
  // could move money, and this page will not offer to sign one.
  //
  // The rest of the shape is checked too, because "harmless" should not rest on
  // a single field: a challenge is manageData operations and nothing else, and
  // the first one is sourced by the account being proved.
  function readChallenge(xdr, networkPassphrase, expectedAddress) {
    const tx = new StellarSdk.TransactionBuilder.fromXDR(xdr, networkPassphrase);

    if (String(tx.sequence) !== "0") {
      throw new Error(
        "this is not a challenge — it has a real sequence number, which means it could be submitted"
      );
    }
    if (!tx.operations.length) {
      throw new Error("this challenge has no operations");
    }
    for (const op of tx.operations) {
      if (op.type !== "manageData") {
        throw new Error("this transaction does something other than a challenge");
      }
    }
    if (tx.operations[0].source !== expectedAddress) {
      throw new Error("this challenge is for a different address");
    }
    return tx;
  }

  function loadDeviceKey() {
    const seed = localStorage.getItem(KEY);
    if (!seed) return null;
    try {
      return StellarSdk.Keypair.fromSecret(seed);
    } catch {
      return null;
    }
  }

  // What each purpose calls itself. Kept in one place so a heading and the
  // message under it cannot drift apart.
  const COPY = {
    [MEMBER]: {
      heading: "Link your wallet",
      lede:
        "Sign this challenge to prove you control the address. It is built so " +
        "that it can never be submitted — signing it cannot move anything.",
      pasteHeading: "Or sign in your own wallet",
      pasteNote:
        "Copy the challenge below, sign it in your wallet, and paste the " +
        "signed result back.",
      done: "Wallet linked",
      doneLede: "You can close this page and go back to your chat.",
    },
    [TREASURY]: {
      heading: "Prove your treasury",
      lede:
        "Sign this challenge to prove your group controls this account. It " +
        "takes the same signing weight a payment takes, and the challenge " +
        "itself can never be submitted — signing it cannot move anything.",
      pasteHeading: "Collect the signatures",
      pasteNote:
        "Copy the challenge below and pass it between signers. Each one adds " +
        "their signature to the same envelope; paste it back once you have " +
        "enough. Separate submissions do not add up.",
      done: "Treasury linked",
      doneLede: "You can close this page and go back to your chat.",
    },
  };

  function explain(err, purpose) {
    if (err.status === 400 && purpose === TREASURY) {
      return [
        "Not enough signing weight yet.",
        "Pass this same envelope to the other signers and paste it back once " +
          "they have added theirs.",
      ];
    }
    switch (err.status) {
      case 401:
        return ["This link has expired.", "Ask stelfin for a new one in your chat."];
      case 404:
        return ["This challenge is no longer valid.", "Ask stelfin for a new one in your chat."];
      case 409:
        return ["That address is already linked.", "It belongs to another member of this workspace."];
      case 400:
        return ["That signature does not prove this address.", "Sign with the key that owns it."];
      case 422:
        return [
          "This account cannot prove control by signature.",
          "Its signers cannot reach the weight it needs to move money.",
        ];
      case 503:
        return ["Linking is not available.", "This deployment has no web-auth key configured."];
      default:
        return ["Something went wrong.", "Nothing was changed — please try again in a moment."];
    }
  }

  async function submit(signedXDR, purpose) {
    try {
      const res = await api("POST", "/v1/link/submit", {
        signed_xdr: signedXDR,
        label: ($("label").value || "").trim(),
      });
      const copy = COPY[res.purpose] || COPY[purpose] || COPY[MEMBER];
      $("doneHeading").textContent = copy.done;
      $("doneLede").textContent =
        res.purpose === TREASURY && Array.isArray(res.signed_by)
          ? "Proved by " +
            res.signed_by.length +
            " of the account's signers, against a threshold of " +
            res.threshold +
            ". You can close this page and go back to your chat."
          : copy.doneLede;
      $("alert").hidden = true;
      $("pending").hidden = true;
      $("done").hidden = false;
      return true;
    } catch (err) {
      const [title, detail] = explain(err, purpose);
      // A treasury envelope short of its threshold leaves the page usable, so
      // the next signature can be pasted into the same box.
      notice(title, detail, err.status === 400 && purpose === TREASURY);
      return false;
    }
  }

  (async () => {
    let data;
    try {
      data = await api("GET", "/v1/link");
    } catch (err) {
      const [title, detail] = explain(err);
      fail(title, detail);
      return;
    }

    // The purpose only chooses wording. The server dispatches on its own stored
    // purpose when the signature arrives, so a tampered value here changes
    // nothing but the headings on this page.
    const purpose = COPY[data.purpose] ? data.purpose : MEMBER;
    const copy = COPY[purpose];
    $("heading").textContent = copy.heading;
    $("lede").textContent = copy.lede;
    $("pasteHeading").textContent = copy.pasteHeading;
    $("pasteNote").textContent = copy.pasteNote;
    $("labelBox").hidden = purpose !== TREASURY;

    let tx;
    try {
      tx = readChallenge(data.xdr, data.network_passphrase, data.address);
    } catch (err) {
      fail("stelfin will not ask you to sign this.", err.message);
      return;
    }

    // textContent throughout: some of what is rendered came from the server,
    // and none of it is markup.
    $("address").textContent = data.address;
    $("sequence").textContent = String(tx.sequence);
    $("network").textContent = data.network_passphrase;
    $("challenge").value = data.xdr;
    $("pending").hidden = false;

    const key = loadDeviceKey();
    if (key && key.publicKey() === data.address) {
      $("device").hidden = false;
      $("sign").addEventListener("click", async () => {
        busy($("sign"), true);
        try {
          const signed = new StellarSdk.TransactionBuilder.fromXDR(
            data.xdr,
            data.network_passphrase
          );
          signed.sign(key);
          await submit(signed.toXDR(), purpose);
        } catch (err) {
          fail("Could not sign on this device.", err.message);
        }
      });
    }

    $("submit").addEventListener("click", async () => {
      const pasted = $("signed").value.trim();
      if (!pasted) {
        fail("Nothing to submit.", "Paste the signed challenge first.");
        return;
      }
      // Check what was pasted before sending it: a signature over something
      // other than this challenge is a mistake worth catching here, where the
      // reason can be explained, rather than as a bare rejection.
      try {
        readChallenge(pasted, data.network_passphrase, data.address);
      } catch (err) {
        fail("That is not this challenge.", err.message);
        return;
      }
      busy($("submit"), true);
      const done = await submit(pasted, purpose);
      // Still short of the threshold: the same box takes the next signature.
      if (!done) busy($("submit"), false);
    });
  })();
})();
