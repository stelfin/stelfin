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
    $("reclaim").hidden = true;
    $("done").hidden = true;
  }

  // Fragment-borne, stripped from the address bar immediately: this token
  // authorises deleting an account, and a screenshot of the URL should not
  // carry it.
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

  function explain(err) {
    switch (err.status) {
      case 401:
        return ["This link has expired.", "Run /reclaim again in your chat."];
      case 404:
        return ["This hand-back is no longer valid.", "Run /reclaim again in your chat."];
      case 422:
        return [
          "This account can't be handed back as it stands.",
          "Something changed since the link was made — run /reclaim again to see why.",
        ];
      case 400:
        return [
          "That signature is not for this envelope.",
          "Sign the envelope shown here, not a copy from an earlier message.",
        ];
      default:
        return ["Something went wrong.", "Nothing was changed — please try again in a moment."];
    }
  }

  function derive(data) {
    const d = StelfinDescribe.describeTx(data.xdr, data.network_passphrase);
    if (d.hash !== data.hash) {
      throw new Error("the envelope is not the transaction this link names");
    }
    // The shape rule. Lives in the browser and is never sent by the server: a
    // rule the server supplied would be a rule the server could relax.
    StelfinPolicy.reclaim(d, data);
    return d;
  }

  function renderOperations(d) {
    const box = $("operations");
    box.innerHTML = "";
    for (const op of d.operations) {
      const section = document.createElement("section");
      section.className = "op";

      const title = document.createElement("h3");
      const idx = document.createElement("span");
      idx.className = "idx";
      idx.textContent = "#" + (op.index + 1) + " ";
      title.appendChild(idx);
      title.appendChild(document.createTextNode(op.type));
      section.appendChild(title);

      if (op.summary) {
        const summary = document.createElement("p");
        summary.className = "note";
        summary.style.marginTop = "0";
        summary.textContent = op.summary;
        section.appendChild(summary);
      }
      box.appendChild(section);
    }
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

  async function submit(signedXDR) {
    try {
      const res = await api("POST", "/v1/reclaim/submit", { signed_xdr: signedXDR });
      $("doneLede").textContent =
        "Account " + res.address + " is gone, in ledger " + res.ledger +
        ". You can close this page and go back to your chat.";
      $("alert").hidden = true;
      $("reclaim").hidden = true;
      $("done").hidden = false;
      return true;
    } catch (err) {
      const [title, detail] = explain(err);
      fail(title, detail);
      return false;
    }
  }

  (async () => {
    let data;
    try {
      data = await api("GET", "/v1/reclaim");
    } catch (err) {
      const [title, detail] = explain(err);
      fail(title, detail);
      return;
    }

    let d;
    try {
      d = derive(data);
    } catch (err) {
      fail("stelfin will not ask you to sign this.", err.message);
      return;
    }

    $("address").textContent = data.address;
    $("destination").textContent = data.destination;
    $("network").textContent = data.network_passphrase;
    $("envelope").value = data.xdr;
    renderOperations(d);

    // Typing the account's tail is deliberate friction. Everything else on this
    // deployment is one tap; deleting an account should not be, and the tail is
    // short enough to copy from the line above and specific enough that it
    // cannot be done from muscle memory on the wrong page.
    const tail = data.address.slice(-6);
    $("tail").textContent = tail;

    const key = loadDeviceKey();
    $("device").hidden = !(key && key.publicKey() === data.address);

    const gate = () => {
      const armed = $("confirm").value.trim().toUpperCase() === tail;
      $("sign").disabled = !armed;
      $("submit").disabled = !armed;
    };
    $("confirm").addEventListener("input", gate);
    gate();

    $("reclaim").hidden = false;

    $("sign").addEventListener("click", async () => {
      busy($("sign"), true);
      try {
        const tx = new StellarSdk.TransactionBuilder.fromXDR(
          data.xdr,
          data.network_passphrase
        );
        tx.sign(key);
        await submit(tx.toXDR());
      } catch (err) {
        fail("Could not sign on this device.", err.message);
      }
    });

    $("submit").addEventListener("click", async () => {
      const pasted = $("signed").value.trim();
      if (!pasted) {
        fail("Nothing to submit.", "Paste the signed envelope first.");
        return;
      }
      // Check what was pasted before sending it. A signature over something
      // else is worth catching here, where the reason can be explained, rather
      // than as a bare rejection.
      try {
        derive({ ...data, xdr: pasted });
      } catch (err) {
        fail("That is not this envelope.", err.message);
        return;
      }
      busy($("submit"), true);
      await submit(pasted);
    });
  })();
})();
