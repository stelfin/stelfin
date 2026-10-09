(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  // aria-busy is the part of "working" a screen reader announces. Only for
  // work in flight.
  const busy = (el, on) => {
    el.disabled = on;
    el.setAttribute("aria-busy", String(on));
  };

  function notice(title, detail, keepGoing) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    // textContent, never innerHTML: some of this is a member's own words.
    box.appendChild(document.createTextNode(detail));
    box.classList.toggle("progress", Boolean(keepGoing));
    box.setAttribute("role", keepGoing ? "status" : "alert");
    box.hidden = false;
    if (!keepGoing) {
      box.focus();
      $("call").hidden = true;
      $("done").hidden = true;
    }
  }

  const fail = (title, detail) => notice(title, detail, false);

  // The token lives in the fragment so it never reaches the server as part of a
  // URL. Stripped from the address bar immediately, kept for the tab.
  const token = StelfinLink.token();
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

  // What each refusal means, and whether the page is still usable after it.
  // Only a signature that was not accepted leaves the call there to be signed
  // again; everything else is the end of this link.
  function explain(err) {
    switch (err.status) {
      case 401:
        return ["This link has expired.", "Ask stelfin again in your chat for a new one.", false];
      case 404:
        return ["This call no longer exists.", "Ask stelfin again in your chat.", false];
      case 409:
        return [
          "This call has already been sent.",
          "It can be sent once. Ask stelfin in your chat where things stand: /proposals shows the treasury's proposals.",
          false,
        ];
      case 410:
        return [
          "This call can no longer be sent.",
          "It was built a few minutes ago, for your wallet and the treasury as they stood then, and one of them has moved on — most often because you signed something else from this wallet in the meantime. Nothing was sent. Ask stelfin again in your chat and it will build a fresh one.",
          false,
        ];
      case 400:
        return [
          "That signature was not accepted.",
          "It has to be this exact transaction, signed by the wallet shown here. Nothing was sent, and you can try again.",
          true,
        ];
      case 422:
        return [
          "The treasury's contract refused it.",
          "It reached the network and the contract said no, which means something changed after this page was made — a vote that closed, most often. Nothing was paid. Ask stelfin in your chat where things stand.",
          false,
        ];
      case 503:
        return ["That could not be sent just now.", "Nothing was sent. Try again in a moment.", true];
      default:
        return ["Something went wrong.", "Nothing is known to have been sent. Ask stelfin in your chat before trying again.", false];
    }
  }

  // xlm writes stroops as XLM, exactly and without the zeros nobody reads.
  function xlm(stroops) {
    return StelfinPolicy.fromStroops(BigInt(stroops)).replace(/\.?0+$/, "");
  }

  function row(dl, label, value, options) {
    const div = document.createElement("div");
    div.className = "row";
    const dt = document.createElement("dt");
    dt.textContent = label;
    const dd = document.createElement("dd");
    if (options && options.code) {
      const code = document.createElement("code");
      code.textContent = value;
      code.setAttribute("translate", "no");
      dd.appendChild(code);
    } else {
      dd.textContent = value;
    }
    if (options && options.note) {
      const small = document.createElement("small");
      small.textContent = options.note;
      dd.appendChild(small);
    }
    div.appendChild(dt);
    div.appendChild(dd);
    dl.appendChild(div);
  }

  // The call as the envelope carries it, value by value. Shown under the
  // reading and never instead of it: the sentence above is this page's, and
  // this is what it was made from.
  function renderOperation(d) {
    const box = $("operations");
    box.innerHTML = "";
    for (const op of d.operations) {
      const section = document.createElement("section");
      section.className = "op";
      const title = document.createElement("h3");
      title.textContent = op.type;
      section.appendChild(title);
      const dl = document.createElement("dl");
      for (const field of op.fields) {
        const div = document.createElement("div");
        div.className = "row";
        const dt = document.createElement("dt");
        dt.textContent = field.label;
        const dd = document.createElement("dd");
        const code = document.createElement("code");
        code.textContent = field.kind === "address" ? short(field.value) : field.value;
        code.title = field.value;
        dd.appendChild(code);
        div.appendChild(dt);
        div.appendChild(dd);
        dl.appendChild(div);
      }
      section.appendChild(dl);
      box.appendChild(section);
    }
  }

  // The checks the page exists for, in the order that makes each one mean
  // something. Agreeing with the server about what the envelope says comes
  // first; whether that is something this page will offer comes second, and is
  // decided here from the envelope alone.
  async function derive(data) {
    const d = StelfinDescribe.describeTx(data.xdr, data.network_passphrase);
    if (StelfinDescribe.canonical(d) !== data.canonical) {
      throw new Error("stelfin's description of this transaction does not match what your browser reads from it");
    }
    if (d.hash !== data.hash) {
      throw new Error("the envelope is not the transaction this link names");
    }

    const reading = StelfinTreasury.read(d, {
      treasury: data.treasury,
      member: data.member,
      tokens: StelfinTreasury.tokens(data.assets, data.network_passphrase),
    });

    // A proposal commits to its proposer's note, and the envelope carries the
    // commitment. The note itself comes from the server, so it is shown as the
    // proposal's own only if it is the one the commitment is to.
    let note = null;
    if (reading.kind === "propose") {
      const ours = await StelfinTreasury.noteCommitment(data.note || "");
      if (ours !== reading.memo) {
        throw new Error("the note stelfin shows for this proposal is not the one the proposal commits to");
      }
      note = data.note || "";
    }
    return { d, reading, note };
  }

  let passphrase = "";
  let member = "";

  function render(data, derived) {
    const { d, reading, note } = derived;
    passphrase = data.network_passphrase;
    member = data.member;

    $("heading").textContent = reading.heading;
    $("lede").textContent = reading.what;

    const dl = $("reading");
    dl.innerHTML = "";
    for (const r of reading.rows) row(dl, r.label, r.value, r);

    if (data.says) {
      $("saidText").textContent = data.says;
      $("saidNote").textContent =
        reading.kind === "vote" || reading.kind === "cancel"
          ? "This page has not checked that. What a proposal does is recorded on the network under its number, and is not in the transaction you are signing — which names only the number."
          : "That is how stelfin put it in your chat. The box above is what this page read for itself.";
      $("said").hidden = false;
    }
    if (note) {
      $("noteText").textContent = note;
      $("noteBox").hidden = false;
    }

    $("treasury").textContent = data.treasury_label ? data.treasury_label + " · " + data.treasury : data.treasury;
    $("member").textContent = data.member;
    $("network").textContent = StelfinChain.name(data.network_passphrase);
    // The fee is in the envelope, and stelfin has said it will pay it by
    // wrapping this in a transaction of its own. Said as both: the first is
    // what this page can see, the second is what stelfin undertakes.
    $("fee").textContent = "up to " + xlm(d.fee) + " XLM, which stelfin pays when it sends this for you";
    $("expires").textContent = d.max_time
      ? new Date(Number(d.max_time) * 1000).toLocaleString()
      : new Date(data.expires_at).toLocaleString();

    renderOperation(d);
    $("envelope").value = data.xdr;
    $("call").hidden = false;
  }

  // What to say once it has landed. Written from what this page read the call
  // to be, with the one thing only the network could add: a new proposal's
  // number.
  function done(reading, res) {
    let heading = "Sent";
    let lede = "It is on the network.";
    switch (reading.kind) {
      case "propose":
        if (res.proposal) {
          heading = "Proposal " + res.proposal + " is open";
          lede =
            "The treasury's voters can vote on it now. Tell them its number: each runs /approve " +
            res.proposal + " in your chat to vote for it, or /reject " + res.proposal +
            " to vote against. You have not voted yet either.";
        } else {
          heading = "Your proposal is open";
          lede = "Run /proposals in your chat to see its number. Each voter, you included, then votes on it with /approve.";
        }
        break;
      case "vote":
        heading = "Your vote is in";
        lede = "The contract has counted it. Run /proposals in your chat to see where the proposal stands.";
        break;
      case "cancel":
        heading = "The proposal is withdrawn";
        lede = "Nothing it proposed will happen.";
        break;
      case "spend":
        heading = "Paid";
        lede = "The treasury has paid it out.";
        break;
      case "deposit":
        heading = "Deposited";
        lede = "It is in the treasury. Run /balance in your chat to see it.";
        break;
    }
    $("doneHeading").textContent = heading;
    $("doneLede").textContent = lede + " Transaction " + res.hash + ", in ledger " + res.ledger + ".";
    StelfinChain.point($("explorer"), passphrase, res.hash);
    $("alert").hidden = true;
    $("call").hidden = true;
    $("done").hidden = false;
    $("doneHeading").setAttribute("tabindex", "-1");
    $("doneHeading").focus();
  }

  (async () => {
    let data;
    let derived;
    try {
      data = await api("GET", "/v1/call");
    } catch (err) {
      const [title, detail] = explain(err);
      fail(title, detail);
      return;
    }
    try {
      derived = await derive(data);
    } catch (err) {
      fail("stelfin will not ask you to sign this.", err.message);
      return;
    }
    render(data, derived);

    // One signature, from the wallet this call is sent from, on exactly this
    // transaction — checked here before anything is sent, whichever way it
    // was made.
    async function send(signedXDR) {
      // The two mistakes a paste invites, each said as what it is. Pasting
      // back what was copied, unsigned, is the commonest thing to do on a page
      // with two boxes.
      let offered;
      try {
        offered = StellarSdk.TransactionBuilder.fromXDR(signedXDR, passphrase);
      } catch (_) {
        notice("That could not be read.",
          "It should be the signed envelope your wallet gave back: one long line of letters and digits. Nothing was sent.", true);
        return false;
      }
      if (!offered.signatures || offered.signatures.length === 0) {
        notice("That envelope is not signed.",
          "It is the one this page gave you, unchanged. Sign it in your wallet, then paste what the wallet gives back. Nothing was sent.", true);
        return false;
      }

      let checked;
      try {
        checked = StelfinWallet.addOne(data.xdr, signedXDR, passphrase, member, member);
      } catch (err) {
        notice(err.title || "That could not be used.", err.message || "Nothing was sent.", true);
        return false;
      }
      try {
        const res = await api("POST", "/v1/call/submit", { signed_xdr: checked.xdr });
        done(derived.reading, res);
        return true;
      } catch (err) {
        const [title, detail, usable] = explain(err);
        notice(title, detail, usable);
        return false;
      }
    }

    // A wallet stelfin set up on this device signs here, with the key this
    // browser already holds for that address.
    const key = StelfinDeviceKey.keyFor(member);
    if (key) {
      $("deviceOffer").hidden = false;
      $("deviceSign").addEventListener("click", async () => {
        busy($("deviceSign"), true);
        try {
          const tx = StellarSdk.TransactionBuilder.fromXDR(data.xdr, passphrase);
          tx.sign(key);
          await send(tx.toXDR());
        } catch (_) {
          notice("This device could not sign it.", "Nothing was sent. Sign in your own wallet below instead.", true);
        } finally {
          busy($("deviceSign"), false);
        }
      });
      $("pasteHeading").textContent = "Or sign in another wallet";
    }

    $("submit").addEventListener("click", async () => {
      const pasted = $("signed_xdr").value.trim();
      if (!pasted) {
        notice("Nothing to send.", "Paste the signed envelope first.", true);
        return;
      }
      busy($("submit"), true);
      await send(pasted);
      busy($("submit"), false);
    });

    StelfinClipboard.wire($("copyEnvelope"), $("envelope"), $("copiedEnvelope"));

    // Freighter is asked for this wallet's signature and no other: a call is
    // made in one account's name, and another account signing it would be
    // turned away by the network.
    StelfinWallet.offer({
      section: $("walletOffer"),
      button: $("walletSign"),
      absent: key ? null : $("noWallet"),
      envelope: () => ({ xdr: data.xdr, passphrase, address: member }),
      signed: async ({ xdr }) => {
        await send(xdr);
      },
      refused: (title, detail) => notice(title, detail, true),
    }).then((there) => {
      if (there) $("pasteHeading").textContent = "Or sign in another wallet";
    });
  })();
})();
