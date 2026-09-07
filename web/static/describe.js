// Re-derive what a transaction does, from the transaction.
//
// The second implementation. The server produces a description and a canonical
// form; this produces the same canonical form from the same XDR, and the page
// compares them byte for byte. A compromised or buggy server can mis-describe a
// payment — it cannot get that description signed, because the description it
// sent is not the one the signer's own browser derived.
//
// Two rules make that comparison worth anything.
//
// Nothing here reads the server's description. Every value comes from the
// envelope. A renderer that peeked would agree with the server by construction
// and prove nothing.
//
// And anything this cannot fully render is refused, exactly as the Go side
// refuses it. A renderer that showed what it understood and skipped the rest
// would let an operation ride along unmentioned, which is the whole attack.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory(require("./stellar-sdk.min.js"), require("./scval.js"));
  } else {
    root.StelfinDescribe = factory(root.StellarSdk, root.StelfinScval);
  }
})(typeof self !== "undefined" ? self : this, function (StellarSdk, StelfinScval) {
  "use strict";

  // Must match settlement/canonical.go. A change to either without the other is
  // caught by the cross-language corpus.
  const CANONICAL_VERSION = "stelfin-tx-description-1";

  class Indescribable extends Error {}

  // normalizeAmount renders an amount with exactly seven decimal places.
  //
  // String arithmetic, never a number. The SDK hands back "5000" where Go
  // produces "5000.0000000" — the same amount, two spellings — and parseFloat
  // would introduce the one error internal/money exists to prevent. Seven
  // decimals is Stellar's precision, so nothing is lost by padding and anything
  // longer is not an amount this chain can hold.
  function normalizeAmount(amount) {
    if (typeof amount !== "string" || amount === "") {
      throw new Indescribable("missing amount");
    }
    let sign = "";
    let rest = amount;
    if (rest[0] === "-") {
      sign = "-";
      rest = rest.slice(1);
    }
    const [whole, fraction = ""] = rest.split(".");
    if (!/^[0-9]+$/.test(whole) || (fraction !== "" && !/^[0-9]+$/.test(fraction))) {
      throw new Indescribable("unreadable amount " + amount);
    }
    if (fraction.length > 7) {
      throw new Indescribable("amount " + amount + " has more precision than Stellar holds");
    }
    return sign + whole + "." + fraction.padEnd(7, "0");
  }

  function describeAsset(asset) {
    if (!asset) throw new Indescribable("operation has no asset");
    if (asset.isNative()) return "native";
    const code = asset.getCode();
    const issuer = asset.getIssuer();
    if (!code || !issuer) throw new Indescribable("asset has no code or issuer");
    return code + ":" + issuer;
  }

  function assetCode(asset) {
    return asset === "native" ? "XLM" : asset.slice(0, asset.indexOf(":"));
  }

  function hexOf(bytes) {
    const digits = "0123456789abcdef";
    let out = "";
    for (const b of bytes) out += digits[b >> 4] + digits[b & 0x0f];
    return out;
  }

  function describePath(path) {
    if (!path || path.length === 0) return "direct";
    return path.map(describeAsset).join(" \u2192 ");
  }

  const FLAG_NAMES = {
    1: "auth required",
    2: "auth revocable",
    4: "auth immutable",
    8: "clawback enabled",
  };

  function flagName(f) {
    return FLAG_NAMES[f] || "flag " + String(f);
  }

  // describeSetOptions mirrors the Go side field for field, including the order.
  //
  // Every part is shown even when only one changed: a threshold left alone next
  // to a master weight set to zero is exactly the shape of a lockout, and a
  // renderer that omitted the unchanged half would hide it.
  function describeSetOptions(op) {
    const fields = [];
    const changes = [];

    if (op.masterWeight !== undefined) {
      fields.push({ label: "master weight", kind: "number", value: String(op.masterWeight) });
      changes.push("master weight");
    }
    for (const [label, value] of [
      ["low threshold", op.lowThreshold],
      ["medium threshold", op.medThreshold],
      ["high threshold", op.highThreshold],
    ]) {
      if (value === undefined) continue;
      fields.push({ label, kind: "number", value: String(value) });
      changes.push(label);
    }
    if (op.signer) {
      const address = op.signer.ed25519PublicKey || op.signer.key;
      if (!address) throw new Indescribable("a signer this renderer cannot name");
      fields.push({ label: "signer", kind: "address", value: address });
      fields.push({ label: "signer weight", kind: "number", value: String(op.signer.weight) });
      changes.push(op.signer.weight === 0 ? "remove a signer" : "add or change a signer");
    }
    if (op.homeDomain !== undefined) {
      fields.push({ label: "home domain", kind: "text", value: op.homeDomain });
      changes.push("home domain");
    }
    if (op.inflationDest !== undefined) {
      fields.push({ label: "inflation destination", kind: "address", value: op.inflationDest });
      changes.push("inflation destination");
    }
    for (const f of op.setFlags || []) {
      fields.push({ label: "set flag", kind: "flag", value: flagName(f) });
      changes.push("set " + flagName(f));
    }
    for (const f of op.clearFlags || []) {
      fields.push({ label: "clear flag", kind: "flag", value: flagName(f) });
      changes.push("clear " + flagName(f));
    }

    if (!fields.length) throw new Indescribable("set_options changes nothing");
    return { fields, summary: "Change account settings: " + changes.join(", ") };
  }

  function describeMemo(memo) {
    if (!memo || memo.type === "none" || memo.type === undefined) {
      return { type: "none", value: "" };
    }
    switch (memo.type) {
      case "text":
        return {
          type: "text",
          value: typeof memo.value === "string" ? memo.value : bytesToUtf8(memo.value),
        };
      case "id":
        return { type: "id", value: String(memo.value) };
      case "hash":
        return { type: "hash", value: hexOf(memo.value) };
      case "return":
        return { type: "return", value: hexOf(memo.value) };
      default:
        throw new Indescribable("memo type " + memo.type);
    }
  }

  function bytesToUtf8(bytes) {
    return new TextDecoder("utf-8", { fatal: false }).decode(Uint8Array.from(bytes));
  }

  // describeOp mirrors settlement/describe.go's switch, case for case.
  //
  // The default is a refusal on both sides, and the set of cases is deliberately
  // the same set: an operation the browser cannot render is one nobody gets to
  // sign here, whatever the server can say about it.
  // priceOf reads an offer's price as the rational it actually is.
  //
  // The parsed operation exposes price as a decimal string —
  // "2.33333333333333333333" for 7/3 — which is precisely the lossy rendering
  // the Go side refuses to produce. Two implementations cannot agree on a value
  // one of them has already rounded, so this reaches past the parsed form to
  // the numerator and denominator in the envelope.
  function priceOf(rawOp) {
    const body = rawOp.body();
    const arm = body.switch().name;
    let op;
    switch (arm) {
      case "manageSellOffer":
        op = body.manageSellOfferOp();
        break;
      case "manageBuyOffer":
        op = body.manageBuyOfferOp();
        break;
      case "createPassiveSellOffer":
        op = body.createPassiveSellOfferOp();
        break;
      default:
        throw new Indescribable("no price on a " + arm);
    }
    const price = op.price();
    return price.n() + "/" + price.d();
  }

  // describeInvoke mirrors settlement/describe_invoke.go.
  //
  // Read from the raw XDR rather than from the SDK's parsed operation. The
  // parsed form hands back convenience objects whose shape has changed between
  // SDK releases; the XDR is the thing being signed and does not move.
  function describeInvoke(rawOp) {
    const ihf = rawOp.body().invokeHostFunctionOp();
    const fn = ihf.hostFunction();
    if (fn.switch().name !== "hostFunctionTypeInvokeContract") {
      // Uploading WASM and creating contracts are deployment acts, not
      // treasury operations. Refused rather than half-rendered.
      throw new Indescribable(
        "a host function of type " + fn.switch().name + " cannot be shown"
      );
    }

    const call = fn.invokeContract();
    const contract = StelfinScval.addressString(call.contractAddress());
    const name = call.functionName().toString();
    const args = call.args();

    const fields = [
      { label: "contract", kind: "address", value: contract },
      { label: "function", kind: "text", value: name },
      // Its own field so a renderer cannot show three arguments out of four
      // and have the description still look complete.
      { label: "arguments", kind: "number", value: String(args.length) },
    ];
    args.forEach((arg, i) => {
      fields.push({ label: "arg " + i, kind: "raw", value: StelfinScval.render(arg) });
    });

    // Who else is being asked to authorise. Left out, a call that moves a
    // third party's tokens would look like a call that moves nothing.
    const auth = ihf.auth();
    fields.push({ label: "authorizations", kind: "number", value: String(auth.length) });
    auth.forEach((entry, i) => {
      fields.push({
        label: "authorized by " + i,
        kind: "address",
        value: authorizerString(entry),
      });
    });

    return {
      fields,
      summary:
        "Call " + name + " on contract " + contract + " with " + args.length + " argument(s)",
    };
  }

  function authorizerString(entry) {
    const credentials = entry.credentials();
    switch (credentials.switch().name) {
      case "sorobanCredentialsSourceAccount":
        // The transaction's own source authorises it, which the envelope
        // already says. Named explicitly so the count and the list agree.
        return "the transaction's source account";
      case "sorobanCredentialsAddress":
        return StelfinScval.addressString(credentials.address().address());
      default:
        throw new Indescribable(
          "a credential of type " + credentials.switch().name + " cannot be shown"
        );
    }
  }

  function describeOp(index, op, txSource, rawOp) {
    const source = op.source || txSource;
    const out = { index, type: null, source, summary: "", fields: [] };

    switch (op.type) {
      case "payment": {
        const amount = normalizeAmount(op.amount);
        const asset = describeAsset(op.asset);
        out.type = "payment";
        out.summary = "Send " + amount + " " + assetCode(asset) + " to " + op.destination;
        out.fields = [
          { label: "amount", kind: "amount", value: amount },
          { label: "asset", kind: "asset", value: asset },
          { label: "destination", kind: "address", value: op.destination },
        ];
        break;
      }

      case "createAccount": {
        const amount = normalizeAmount(op.startingBalance);
        out.type = "create_account";
        out.summary = "Create account " + op.destination + " with " + amount + " XLM";
        out.fields = [
          { label: "destination", kind: "address", value: op.destination },
          { label: "starting balance", kind: "amount", value: amount },
        ];
        break;
      }

      case "accountMerge": {
        out.type = "account_merge";
        out.summary = "Merge this account into " + op.destination + ", closing it";
        out.fields = [{ label: "destination", kind: "address", value: op.destination }];
        break;
      }

      case "changeTrust": {
        const asset = describeAsset(op.line);
        const limit = normalizeAmount(op.limit);
        out.type = "change_trust";
        out.summary =
          limit === "0.0000000"
            ? "Remove the trustline for " + assetCode(asset)
            : "Trust " + assetCode(asset) + " up to " + limit;
        out.fields = [
          { label: "asset", kind: "asset", value: asset },
          { label: "limit", kind: "amount", value: limit },
        ];
        break;
      }

      case "beginSponsoringFutureReserves": {
        out.type = "begin_sponsoring_future_reserves";
        out.summary = "Pay the reserves " + op.sponsoredId + " is about to need";
        out.fields = [{ label: "sponsored", kind: "address", value: op.sponsoredId }];
        break;
      }

      case "endSponsoringFutureReserves": {
        out.type = "end_sponsoring_future_reserves";
        out.summary = "Stop sponsoring further reserves";
        break;
      }

      case "bumpSequence": {
        out.type = "bump_sequence";
        out.summary = "Bump the sequence number to " + op.bumpTo;
        out.fields = [{ label: "bump to", kind: "number", value: String(op.bumpTo) }];
        break;
      }

      case "manageData": {
        out.type = "manage_data";
        if (op.value === null || op.value === undefined) {
          out.summary = 'Delete the data entry "' + op.name + '"';
          out.fields = [{ label: "name", kind: "text", value: op.name }];
        } else {
          out.summary = 'Set the data entry "' + op.name + '"';
          out.fields = [
            { label: "name", kind: "text", value: op.name },
            { label: "value", kind: "raw", value: hexOf(op.value) },
          ];
        }
        break;
      }

      case "manageSellOffer":
      case "manageBuyOffer":
      case "createPassiveSellOffer": {
        const verb =
          op.type === "manageBuyOffer"
            ? "Buy"
            : op.type === "createPassiveSellOffer"
              ? "Passively sell"
              : "Sell";
        const amount = normalizeAmount(op.amount);
        const selling = describeAsset(op.selling);
        const buying = describeAsset(op.buying);
        const rate = priceOf(rawOp);
        const offerId = op.offerId === undefined ? "0" : String(op.offerId);

        out.type =
          op.type === "manageBuyOffer"
            ? "manage_buy_offer"
            : op.type === "createPassiveSellOffer"
              ? "create_passive_sell_offer"
              : "manage_sell_offer";
        out.summary =
          amount === "0.0000000"
            ? "Cancel offer " + offerId
            : verb +
              " " +
              amount +
              " " +
              assetCode(selling) +
              " for " +
              assetCode(buying) +
              " at " +
              rate;
        out.fields = [
          { label: "selling", kind: "asset", value: selling },
          { label: "buying", kind: "asset", value: buying },
          { label: "amount", kind: "amount", value: amount },
          { label: "price", kind: "price", value: rate },
          { label: "offer", kind: "number", value: offerId },
        ];
        break;
      }

      case "pathPaymentStrictSend": {
        const send = normalizeAmount(op.sendAmount);
        // The floor, not a quote. What the signature commits to is the worst
        // outcome, and that is the number a person needs in front of them.
        const destMin = normalizeAmount(op.destMin);
        const sendAsset = describeAsset(op.sendAsset);
        const destAsset = describeAsset(op.destAsset);
        out.type = "path_payment_strict_send";
        out.summary =
          "Send " + send + " " + assetCode(sendAsset) + " to " + op.destination +
          ", who receives at least " + destMin + " " + assetCode(destAsset);
        out.fields = [
          { label: "send amount", kind: "amount", value: send },
          { label: "send asset", kind: "asset", value: sendAsset },
          { label: "destination", kind: "address", value: op.destination },
          { label: "destination asset", kind: "asset", value: destAsset },
          { label: "minimum received", kind: "amount", value: destMin },
          { label: "path", kind: "raw", value: describePath(op.path) },
        ];
        break;
      }

      case "pathPaymentStrictReceive": {
        const sendMax = normalizeAmount(op.sendMax);
        const destAmount = normalizeAmount(op.destAmount);
        const sendAsset = describeAsset(op.sendAsset);
        const destAsset = describeAsset(op.destAsset);
        out.type = "path_payment_strict_receive";
        out.summary =
          "Send " + op.destination + " at most " + sendMax + " " + assetCode(sendAsset) +
          " so they receive " + destAmount + " " + assetCode(destAsset);
        out.fields = [
          { label: "maximum sent", kind: "amount", value: sendMax },
          { label: "send asset", kind: "asset", value: sendAsset },
          { label: "destination", kind: "address", value: op.destination },
          { label: "destination asset", kind: "asset", value: destAsset },
          { label: "received", kind: "amount", value: destAmount },
          { label: "path", kind: "raw", value: describePath(op.path) },
        ];
        break;
      }

      case "setOptions": {
        out.type = "set_options";
        const { fields, summary } = describeSetOptions(op);
        out.summary = summary;
        out.fields = fields;
        break;
      }

      case "invokeHostFunction": {
        out.type = "invoke_contract";
        const { fields, summary } = describeInvoke(rawOp);
        out.fields = fields;
        out.summary = summary;
        break;
      }

      default:
        // Any operation neither side renders. Refusing is the safe direction:
        // an operation nobody can read is one nobody gets to sign.
        throw new Indescribable("operation " + index + " is a " + op.type);
    }

    return out;
  }

  // describeTx re-derives the whole description from an envelope.
  function describeTx(xdr, networkPassphrase) {
    const tx = StellarSdk.TransactionBuilder.fromXDR(xdr, networkPassphrase);
    if (tx.operations === undefined) {
      throw new Indescribable("not a simple transaction");
    }

    const source = tx.source;
    const memo = describeMemo(tx.memo);
    const bounds = tx.timeBounds || {};

    const description = {
      kind: "tx",
      network: networkPassphrase,
      hash: tx.hash().toString("hex"),
      source,
      sequence: String(tx.sequence),
      fee: String(tx.fee),
      min_time: bounds.minTime && String(bounds.minTime) !== "0" ? String(bounds.minTime) : "",
      max_time: bounds.maxTime && String(bounds.maxTime) !== "0" ? String(bounds.maxTime) : "",
      memo_type: memo.type,
      memo: memo.value,
      operations: [],
    };

    if (!tx.operations.length) throw new Indescribable("no operations");
    const raw = tx.tx.operations();
    tx.operations.forEach((op, i) => {
      description.operations.push(describeOp(i, op, source, raw[i]));
    });
    return description;
  }

  // escape mirrors settlement/canonical.go. Backslash first, or an escaped
  // newline would be re-escaped into something that decodes wrongly.
  function escape(value) {
    return String(value)
      .split("\\")
      .join("\\\\")
      .split("\n")
      .join("\\n")
      .split("\r")
      .join("\\r")
      .split("\t")
      .join("\\t");
  }

  // canonical renders the description as the bytes to compare.
  function canonical(d) {
    const lines = [];
    const line = (...parts) => lines.push(parts.map(escape).join("\t"));

    line(CANONICAL_VERSION);
    line("kind", d.kind);
    line("network", d.network);
    line("source", d.source);
    line("sequence", d.sequence);
    line("fee", d.fee);
    line("min_time", d.min_time);
    line("max_time", d.max_time);
    line("memo", d.memo_type, d.memo);
    line("operations", String(d.operations.length));

    for (const op of d.operations) {
      const index = String(op.index);
      line("op", index, op.type, op.source);
      for (const f of op.fields) {
        line("field", index, f.label, f.kind, f.value);
      }
    }

    line("hash", d.hash);
    return lines.join("\n") + "\n";
  }

  return { describeTx, canonical, normalizeAmount, Indescribable, CANONICAL_VERSION };
});
