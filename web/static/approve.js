(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  function notice(title, detail, keepGoing) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    // textContent, never innerHTML: some of this originates with a member's own
    // memo and must never be interpreted as markup.
    box.appendChild(document.createTextNode(detail));
    box.classList.toggle("progress", Boolean(keepGoing));
    box.hidden = false;
    if (!keepGoing) {
      $("proposal").hidden = true;
      $("done").hidden = true;
    }
  }

  const fail = (title, detail) => notice(title, detail, false);

  // The token lives in the fragment so it never reaches the server as part of a
  // URL, and so it stays out of access logs and Referer headers. Stripped from
  // the address bar immediately.
  const token = location.hash.replace(/^#/, "");
  history.replaceState(null, "", location.pathname);

  if (!token) {
    fail("This link is incomplete.", "Open the link from your chat message again.");
    return;
  }

  const short = (s) => (s && s.length > 14 ? s.slice(0, 6) + "…" + s.slice(-6) : s || "");

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
        return ["This link has expired.", "Ask stelfin for a new one in your chat."];
      case 404:
        return ["This proposal no longer exists.", "Ask stelfin for the current one."];
      case 409:
        return [
          "This proposal is not ready, or no longer open.",
          "Reload to see where it stands — something changed since this page loaded.",
        ];
      case 400:
        return [
          "That signature is not for this proposal.",
          "Sign the envelope shown here, not a copy from an earlier message.",
        ];
      case 503:
        return ["Approvals are not available.", "This deployment has not enabled them."];
      default:
        return ["Something went wrong.", "Nothing was changed — please try again in a moment."];
    }
  }

  // The check the whole page exists for.
  //
  // Derive the description from the envelope, render the canonical form, and
  // compare it to the server's byte for byte. Nothing below reads the server's
  // description for display, so a disagreement here means the two implement
  // different opinions about what this transaction does — and the correct
  // response to that is to sign nothing.
  function derive(data) {
    const d = StelfinDescribe.describeTx(data.xdr, data.network_passphrase);
    const ours = StelfinDescribe.canonical(d);
    if (ours !== data.canonical) {
      throw new Error(
        "stelfin's description of this transaction does not match what your " +
          "browser reads from it"
      );
    }
    if (d.hash !== data.hash) {
      throw new Error("the envelope is not the transaction this proposal names");
    }
    return d;
  }

  // A batch is shown as one number and a list, because that is how it is read.
  //
  // The total is computed here from the operations. The server sends one too;
  // this page does not use it. A page that displayed the server's figure and
  // verified its own quietly would show the wrong number on exactly the day the
  // check was the part that broke.
  function renderBatch(d, data) {
    const box = $("batch");
    if (d.operations.length < 2) {
      box.hidden = true;
      return;
    }

    let summary;
    try {
      summary = StelfinPolicy.batch(d, { source: d.source });
    } catch (err) {
      // Not a batch by this page's rules — a mixed-asset envelope, or one with
      // something other than payments in it. Shown row by row instead, with no
      // total, because a total that does not describe everything present is
      // worse than none.
      $("batchNote").textContent =
        "No total is shown: " + err.message + ". Read every row below.";
      $("batchTotal").textContent = "—";
      $("batchRows").textContent = String(d.operations.length);
      box.hidden = false;
      return;
    }

    $("batchTotal").textContent = summary.total + " " + assetCode(summary.asset);
    $("batchRows").textContent = String(summary.rows);
    $("batchNote").textContent =
      "Your browser added this up from the " + summary.rows +
      " payments below. It is not a figure stelfin sent.";
    box.hidden = false;
    void data;
  }

  function assetCode(asset) {
    if (!asset || asset === "native") return "XLM";
    const at = asset.indexOf(":");
    return at === -1 ? asset : asset.slice(0, at);
  }

  function renderPeople(id, addresses, emptyText) {
    const list = $(id);
    list.innerHTML = "";
    if (!addresses || !addresses.length) {
      const li = document.createElement("li");
      li.className = "note";
      li.textContent = emptyText;
      list.appendChild(li);
      return;
    }
    for (const address of addresses) {
      const li = document.createElement("li");
      const code = document.createElement("code");
      code.textContent = address;
      li.appendChild(code);
      list.appendChild(li);
    }
  }

  // Every operation is drawn in its own box. A five-operation payroll must not
  // read as one payment with a long description.
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

      // The one-line summary comes from the same local derivation as the
      // fields below it, so it cannot say something the fields contradict.
      if (op.summary) {
        const summary = document.createElement("p");
        summary.className = "note";
        summary.style.marginTop = "0";
        summary.textContent = op.summary;
        section.appendChild(summary);
      }

      const dl = document.createElement("dl");
      dl.style.borderTop = "0";
      dl.style.margin = "0";
      dl.style.paddingTop = "0";
      for (const field of op.fields) {
        const row = document.createElement("div");
        row.className = "row";
        const dt = document.createElement("dt");
        dt.textContent = field.label;
        const dd = document.createElement("dd");
        const code = document.createElement("code");
        code.textContent = field.kind === "address" ? short(field.value) : field.value;
        code.title = field.value;
        dd.appendChild(code);
        row.appendChild(dt);
        row.appendChild(dd);
        dl.appendChild(row);
      }
      section.appendChild(dl);
      box.appendChild(section);
    }
  }

  function render(data) {
    let d;
    try {
      d = derive(data);
    } catch (err) {
      fail("stelfin will not ask you to sign this.", err.message);
      return null;
    }

    $("lede").textContent =
      "Signing commits you to exactly what is shown below, which your browser " +
      "read from the envelope itself.";
    $("treasury").textContent =
      data.treasury_label ? data.treasury_label + " · " + data.treasury : data.treasury;
    $("weight").textContent = data.have + " of " + data.need;
    $("expires").textContent = new Date(data.expires_at).toLocaleString();
    $("fee").textContent = d.fee + " stroops";
    $("memo").textContent = d.memo_type === "none" ? "none" : d.memo_type + ": " + d.memo;
    $("sequence").textContent = d.sequence;

    renderBatch(d, data);
    renderOperations(d);
    renderPeople("signed", data.signed, "Nobody yet.");
    renderPeople("missing", data.missing, "Nobody — it has every signature.");

    $("envelope").value = data.xdr;
    $("ready").hidden = !data.ready;
    $("proposal").hidden = false;
    return d;
  }

  async function refresh(signedXDR) {
    try {
      const data = signedXDR
        ? await api("POST", "/v1/proposal/approve", { signed_xdr: signedXDR })
        : await api("GET", "/v1/proposal");
      $("alert").hidden = true;
      return render(data);
    } catch (err) {
      const [title, detail] = explain(err);
      // A rejected signature leaves the proposal itself intact, so the page
      // stays usable and the next attempt can be pasted into the same box.
      notice(title, detail, err.status === 400 && !$("proposal").hidden);
      return null;
    }
  }

  (async () => {
    if (!(await refresh(null))) return;

    $("submit").addEventListener("click", async () => {
      const pasted = $("signed_xdr").value.trim();
      if (!pasted) {
        notice("Nothing to submit.", "Paste the signed envelope first.", true);
        return;
      }
      $("submit").disabled = true;
      const ok = await refresh(pasted);
      if (ok) $("signed_xdr").value = "";
      $("submit").disabled = false;
    });

    $("execute").addEventListener("click", async () => {
      $("execute").disabled = true;
      try {
        const res = await api("POST", "/v1/proposal/execute");
        $("doneHeading").textContent = "Submitted to the network";
        $("doneLede").textContent =
          "Transaction " + res.hash + ", in ledger " + res.ledger +
          ". You can close this page and go back to your chat.";
        $("alert").hidden = true;
        $("proposal").hidden = true;
        $("done").hidden = false;
      } catch (err) {
        const [title, detail] = explain(err);
        fail(title, detail);
      }
    });
  })();
})();
