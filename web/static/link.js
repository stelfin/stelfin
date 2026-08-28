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

  function explain(err) {
    switch (err.status) {
      case 401:
        return ["This link has expired.", "Ask stelfin for a new one in your chat."];
      case 404:
        return ["This challenge is no longer valid.", "Ask stelfin for a new one in your chat."];
      case 409:
        return ["That address is already linked.", "It belongs to another member of this workspace."];
      case 400:
        return ["That signature does not prove this address.", "Sign with the key that owns it."];
      case 503:
        return ["Linking is not available.", "This deployment has no web-auth key configured."];
      default:
        return ["Something went wrong.", "Nothing was changed — please try again in a moment."];
    }
  }

  async function submit(signedXDR) {
    try {
      await api("POST", "/v1/link/submit", { signed_xdr: signedXDR });
      $("pending").hidden = true;
      $("done").hidden = false;
    } catch (err) {
      const [title, detail] = explain(err);
      fail(title, detail);
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
        $("sign").disabled = true;
        try {
          const signed = new StellarSdk.TransactionBuilder.fromXDR(
            data.xdr,
            data.network_passphrase
          );
          signed.sign(key);
          await submit(signed.toXDR());
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
      $("submit").disabled = true;
      await submit(pasted);
    });
  })();
})();
