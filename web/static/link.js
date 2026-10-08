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
  const GRANT = "grant_connector";
  const SETUP = "connector_setup";

  // The data entry a statement's digest travels in. A challenge that carries
  // one is asking for more than proof of control, whatever it is labelled.
  const STATEMENT_KEY = "stelfin statement";

  // Several signers pass one envelope between them for these two, so a
  // signature that is not yet enough is progress and not a refusal.
  const collected = (purpose) => purpose === TREASURY || purpose === GRANT;

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
    const tx = StellarSdk.TransactionBuilder.fromXDR(xdr, networkPassphrase);

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
    [GRANT]: {
      heading: "Grant a connector",
      lede:
        "The signers of this treasury are being asked to let stelfin use a " +
        "connector. It takes the same signatures a payment takes. The " +
        "envelope can never be submitted — signing it cannot move anything.",
      pasteHeading: "Sign in your wallet",
      pasteNote:
        "Copy the envelope below, sign it, and pass it on. Each signer adds " +
        "to the same envelope; paste it back once it has enough signatures.",
      done: "Connector granted",
      doneLede: "You can close this page and go back to your chat.",
    },
  };

  // statementIn returns the digest a challenge commits to, or null.
  //
  // Read out of the envelope, which is the thing being signed. More than one
  // such entry is refused: a signature would then commit to two statements and
  // this page can show only one.
  function statementIn(tx) {
    const entries = tx.operations.filter((op) => op.name === STATEMENT_KEY);
    if (entries.length === 0) return null;
    if (entries.length > 1) {
      throw new Error("this challenge commits to more than one statement");
    }
    const value = entries[0].value;
    return value ? new TextDecoder().decode(value) : "";
  }

  async function sha256Hex(text) {
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
    return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
  }

  // What each line of a statement is called on the page. Anything not listed
  // is still shown, under its own key: a line the page does not recognise is a
  // line the signer most needs to see.
  const STATEMENT_LABELS = {
    workspace: "Workspace",
    network: "Network",
    treasury: "Treasury",
    connector: "Connector",
    kind: "Kind",
    reaches: "Reaches",
    endpoint_digest: "Address digest",
    surface: "Surface digest",
    may: "May",
    rate: "Limit",
    cannot: "Cannot",
  };

  // renderStatement draws the statement, line for line, from the same text
  // that was hashed. Nothing here is a summary and nothing is left out.
  function renderStatement(statement) {
    const list = $("statement");
    list.innerHTML = "";
    const lines = statement.split("\n").filter(Boolean);
    for (const line of lines.slice(1)) {
      const tab = line.indexOf("\t");
      const key = tab === -1 ? line : line.slice(0, tab);
      const value = tab === -1 ? "" : line.slice(tab + 1);
      const row = document.createElement("div");
      row.className = "row";
      const dt = document.createElement("dt");
      dt.textContent = STATEMENT_LABELS[key] || key;
      const dd = document.createElement("dd");
      if (key === "cannot" || key === "rate" || key === "kind" || key === "reaches") {
        dd.textContent = value;
      } else {
        const code = document.createElement("code");
        code.translate = false;
        code.textContent = value;
        dd.appendChild(code);
      }
      row.appendChild(dt);
      row.appendChild(dd);
      list.appendChild(row);
    }
  }

  // A mistake the person can put right on this page. It is an error and reads
  // as one, but it leaves the form where it is: hiding the box somebody just
  // mistyped into would turn a typo into a dead end.
  function correctable(title, detail) {
    const box = $("alert");
    box.innerHTML = "";
    const strong = document.createElement("strong");
    strong.textContent = title;
    box.appendChild(strong);
    box.appendChild(document.createTextNode(detail));
    box.classList.remove("progress");
    box.setAttribute("role", "alert");
    box.hidden = false;
    box.focus();
  }

  // setUp is the other thing this page does: no challenge, nothing to sign,
  // only a connector to describe and an address and token to take.
  function setUp(connector) {
    $("setupHeading").textContent = "Set up " + connector.name;
    $("setupName").textContent = connector.name;
    $("setupKind").textContent =
      connector.kind === "mcp" ? "MCP server" : connector.kind === "webhook" ? "Webhook" : connector.kind;
    if (connector.ready) {
      $("saveNote").textContent =
        "This connector is already set up. Saving replaces its address and token, " +
        "and ends any grant it has: the signers agreed to it as it was.";
    }
    $("setup").hidden = false;

    $("save").addEventListener("click", async () => {
      const endpoint = $("endpoint").value.trim();
      if (!endpoint) {
        correctable("Where is it?", "Enter the connector's https address.");
        return;
      }
      busy($("save"), true);
      try {
        const res = await api("POST", "/v1/connector/setup", {
          endpoint,
          credential: $("credential").value,
        });
        // Gone from the page as soon as it has been sent.
        $("endpoint").value = "";
        $("credential").value = "";
        $("doneHeading").textContent = res.name + " is set up";
        const found =
          Array.isArray(res.tools) && res.tools.length
            ? "It offers " + res.tools.length + " tool(s): " + res.tools.join(", ") + ". "
            : "It answered with " + res.rows + " row(s). ";
        $("doneLede").textContent =
          found + "It is not granted yet. Back in your chat: /connector grant " + res.name;
        $("alert").hidden = true;
        $("setup").hidden = true;
        $("done").hidden = false;
      } catch (err) {
        busy($("save"), false);
        if (err.status === 400) {
          correctable("That address cannot be used.", "It must be https and on the public internet.");
        } else if (err.status === 422) {
          correctable(
            "It could not be reached, or did not answer usefully.",
            "Check the address and the token. Nothing was saved."
          );
        } else if (err.status === 401) {
          fail("This link has expired.", "Ask stelfin for a new one: /connector setup " + connector.name);
        } else if (err.status === 503) {
          fail("Connectors are not available.", "This deployment has not been configured for them.");
        } else {
          correctable("Something went wrong.", "Nothing was saved — please try again in a moment.");
        }
      }
    });
  }

  function explain(err, purpose) {
    if (err.status === 409 && purpose === GRANT) {
      return [
        "The connector has changed since this was requested.",
        "What the signers were shown no longer describes it. Ask stelfin for a new grant in your chat.",
      ];
    }
    if (err.status === 400 && collected(purpose)) {
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
      const weighed = collected(res.purpose) && Array.isArray(res.signed_by);
      $("doneLede").textContent = !weighed
        ? copy.doneLede
        : (res.purpose === GRANT ? "Signed by " : "Proved by ") +
          res.signed_by.length +
          " of the account's signers, against a threshold of " +
          res.threshold +
          (res.purpose === GRANT ? ". stelfin may now use " + res.connector + " as stated" : "") +
          ". You can close this page and go back to your chat.";
      $("alert").hidden = true;
      $("pending").hidden = true;
      $("done").hidden = false;
      return true;
    } catch (err) {
      const [title, detail] = explain(err, purpose);
      // A treasury envelope short of its threshold leaves the page usable, so
      // the next signature can be pasted into the same box.
      notice(title, detail, err.status === 400 && collected(purpose));
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

    if (data.purpose === SETUP && data.connector) {
      setUp(data.connector);
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

    // A challenge that commits to a statement is an agreement to something.
    // The rule is decided by the envelope and not by what the server called
    // it: if a digest is in there, this page must have hashed a text to that
    // same digest and be showing that text — or it offers nothing to sign. A
    // server that labelled a grant "link your wallet" would otherwise get one
    // signed by somebody who thought they were proving an address.
    let committed;
    try {
      committed = statementIn(tx);
      if (committed !== null) {
        if (!data.statement) {
          throw new Error("this challenge commits to a statement that was not shown");
        }
        if ((await sha256Hex(data.statement)) !== committed) {
          throw new Error("the statement shown is not the one this challenge commits to");
        }
      } else if (data.statement || purpose === GRANT) {
        throw new Error("this grant's challenge commits to no statement");
      }
    } catch (err) {
      fail("stelfin will not ask you to sign this.", err.message);
      return;
    }
    if (committed !== null) {
      renderStatement(data.statement);
      $("grant").hidden = false;
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
          const signed = StellarSdk.TransactionBuilder.fromXDR(
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
        const returned = readChallenge(pasted, data.network_passphrase, data.address);
        // The hash covers everything but the signatures, so the same hash is
        // the same challenge — the one whose statement, if it has one, is the
        // one shown above. A different challenge for the same address would
        // pass every shape check and commit its signers to something else.
        if (returned.hash().toString("hex") !== tx.hash().toString("hex")) {
          throw new Error("it is a different challenge from the one shown here");
        }
      } catch (err) {
        correctable("That is not this challenge.", err.message);
        return;
      }
      busy($("submit"), true);
      const done = await submit(pasted, purpose);
      // Still short of the threshold: the same box takes the next signature.
      if (!done) busy($("submit"), false);
    });
  })();
})();
