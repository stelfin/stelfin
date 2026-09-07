// Render a Soroban value as text, byte-for-byte as settlement/scval.go does.
//
// The second implementation, and the one that decides what a person actually
// signs. Everything here mirrors the Go side exactly: the same tags, the same
// escaping, the same refusals. A value the Go side renders and this one does
// not — or renders differently — makes the canonical forms disagree, and the
// page refuses to show a sign button. That is the intended outcome, not a bug
// to work around.
//
// The wide integers are the reason this file cannot be casual. A token balance
// is an i128 and JavaScript's Number holds 53 bits of it, so every one of them
// goes through BigInt and none through arithmetic that could round.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory(require("./stellar-sdk.min.js"));
  } else {
    root.StelfinScval = factory(root.StellarSdk);
  }
})(typeof self !== "undefined" ? self : this, function (StellarSdk) {
  "use strict";

  function Indescribable(message) {
    this.name = "Indescribable";
    this.message = message;
  }
  Indescribable.prototype = Object.create(Error.prototype);

  // Must match scvalDelimiters in settlement/scval.go.
  const DELIMITERS = ",[]{}=";

  function hexOf(bytes) {
    let out = "";
    for (const b of new Uint8Array(bytes)) out += b.toString(16).padStart(2, "0");
    return out;
  }

  // escape mirrors escapeScval in settlement/scval.go, including the order:
  // backslash first, or an escaped delimiter would be re-escaped into something
  // that decodes differently.
  function escape(value) {
    let out = "";
    for (const ch of String(value)) {
      const code = ch.codePointAt(0);
      if (ch === "\\") out += "\\\\";
      else if (ch === "\n") out += "\\n";
      else if (ch === "\r") out += "\\r";
      else if (ch === "\t") out += "\\t";
      else if (code < 0x20 || DELIMITERS.includes(ch)) {
        out += "\\u" + code.toString(16).padStart(4, "0");
      } else out += ch;
    }
    return out;
  }

  // Assembling the wide integers from their parts.
  //
  // A u128 is two unsigned 64-bit halves. An i128 is a signed high half with an
  // unsigned low one, two's complement across the whole width — which is why
  // the low half is added rather than or-ed: for a negative high half those are
  // not the same operation.
  const TWO64 = 1n << 64n;

  function bigFromParts(parts, signedHigh) {
    let n = signedHigh ? BigInt.asIntN(64, BigInt(parts[0])) : BigInt(parts[0]);
    for (let i = 1; i < parts.length; i++) {
      n = n * TWO64 + BigInt(parts[i]);
    }
    return n.toString();
  }

  // The SDK exposes these parts as objects with _attributes; reading them
  // through the accessors keeps this working across its internal shapes.
  function u64(value) {
    // xdr.Uint64 / Hyper. toString() on it is exact; Number() would not be.
    return BigInt(value.toString());
  }

  function addressString(address) {
    switch (address.switch().name) {
      case "scAddressTypeAccount":
        return StellarSdk.StrKey.encodeEd25519PublicKey(
          address.accountId().ed25519()
        );
      case "scAddressTypeContract":
        return StellarSdk.StrKey.encodeContract(contractIdBytes(address));
      default:
        throw new Indescribable(
          "an address of type " + address.switch().name + " cannot be shown"
        );
    }
  }

  // The contract id arrives as raw bytes under a name that has changed between
  // SDK versions. Tried in order rather than assumed, because guessing wrong
  // here renders a different contract than the one being called.
  function contractIdBytes(address) {
    const raw = address.contractId();
    if (raw && typeof raw === "object" && typeof raw.length === "number") return raw;
    if (raw && typeof raw.contractId === "function") return raw.contractId();
    throw new Indescribable("this contract address cannot be read");
  }

  // render is the whole surface: one Soroban value in, its canonical text out.
  function render(v) {
    switch (v.switch().name) {
      case "scvBool":
        return "bool:" + (v.b() ? "true" : "false");
      case "scvVoid":
        return "void:";

      case "scvU32":
        return "u32:" + BigInt(v.u32()).toString();
      case "scvI32":
        return "i32:" + BigInt(v.i32()).toString();
      case "scvU64":
        return "u64:" + u64(v.u64()).toString();
      case "scvI64":
        return "i64:" + BigInt(v.i64().toString()).toString();

      case "scvTimepoint":
        return "timepoint:" + u64(v.timepoint()).toString();
      case "scvDuration":
        return "duration:" + u64(v.duration()).toString();

      case "scvU128": {
        const p = v.u128();
        return "u128:" + bigFromParts([p.hi().toString(), p.lo().toString()], false);
      }
      case "scvI128": {
        const p = v.i128();
        return "i128:" + bigFromParts([p.hi().toString(), p.lo().toString()], true);
      }
      case "scvU256": {
        const p = v.u256();
        return (
          "u256:" +
          bigFromParts(
            [p.hiHi().toString(), p.hiLo().toString(), p.loHi().toString(), p.loLo().toString()],
            false
          )
        );
      }
      case "scvI256": {
        const p = v.i256();
        return (
          "i256:" +
          bigFromParts(
            [p.hiHi().toString(), p.hiLo().toString(), p.loHi().toString(), p.loLo().toString()],
            true
          )
        );
      }

      case "scvSymbol":
        return "sym:" + escape(v.sym().toString());
      case "scvString":
        return "str:" + escape(v.str().toString());
      case "scvBytes":
        return "bytes:" + hexOf(v.bytes());

      case "scvAddress":
        return "addr:" + addressString(v.address());

      case "scvVec": {
        const items = v.vec() || [];
        return "vec:[" + items.map(render).join(",") + "]";
      }

      case "scvMap": {
        const entries = v.map() || [];
        // In XDR order, which the protocol already sorts. Sorting here would
        // silently repair a map that was not sorted, hiding an envelope the
        // network is going to reject anyway.
        return (
          "map:{" +
          entries.map((e) => render(e.key()) + "=" + render(e.val())).join(",") +
          "}"
        );
      }

      default:
        // Ledger keys, contract instances, nonce keys: values that live in
        // footprints rather than arguments. Refused rather than summarised,
        // because a value nobody can read is a value nobody reviewed.
        throw new Indescribable(
          "a Soroban value of type " + v.switch().name + " cannot be shown"
        );
    }
  }

  return { render, addressString, escape, Indescribable };
});
