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
    module.exports = factory(require("./stellar-sdk.min.js"));
  } else {
    root.StelfinDescribe = factory(root.StellarSdk);
  }
})(typeof self !== "undefined" ? self : this, function (StellarSdk) {
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
  function describeOp(index, op, txSource) {
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

      default:
        // Offers, path payments, set_options and everything Soroban land here.
        // The Go renderer describes some of those; this one does not yet, and
        // until it does they cannot be signed through a stelfin page. Refusing
        // is the safe direction, and the corpus records which is which.
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
    tx.operations.forEach((op, i) => {
      description.operations.push(describeOp(i, op, source));
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
