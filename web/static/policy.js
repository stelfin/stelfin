// What shape a page will let you sign.
//
// The description says what a transaction does; it does not say whether that is
// the thing you came here for. An envelope that swept a balance to the wrong
// place, or slipped a payment in beside a merge, would describe itself
// perfectly accurately — so the description is not the defence. This is.
//
// These rules live in the browser and are never sent by the server. A rule the
// server supplied would be a rule the server could relax, which is the same as
// having no rule on the day it matters. Each page names the one it enforces,
// and anything a rule does not explicitly permit is refused.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory();
  } else {
    root.StelfinPolicy = factory();
  }
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  function Refused(message) {
    this.name = "Refused";
    this.message = message;
  }
  Refused.prototype = Object.create(Error.prototype);

  function field(op, label) {
    return op.fields.find(function (f) {
      return f.label === label;
    });
  }

  // reclaim: trustline removals, then exactly one merge, and nothing else.
  //
  // Every clause is load-bearing. A trustline set to a non-zero limit is not a
  // removal and leaves the reserve locked. A second merge cannot execute, but a
  // merge to a different destination can, and that is the whole account. An
  // operation of any other type is an operation nobody described to the person
  // signing.
  function reclaim(d, expected) {
    if (d.source !== expected.address) {
      throw new Refused("it is not sent from the account being handed back");
    }

    var merges = 0;
    for (var i = 0; i < d.operations.length; i++) {
      var op = d.operations[i];

      if (op.source && op.source !== expected.address) {
        throw new Refused("one of its operations comes from a different account");
      }

      if (op.type === "change_trust") {
        var limit = field(op, "limit");
        if (!limit || Number(limit.value) !== 0) {
          throw new Refused("it changes a trustline instead of removing one");
        }
        continue;
      }

      if (op.type === "account_merge") {
        merges += 1;
        var to = field(op, "destination");
        if (!to || to.value !== expected.destination) {
          throw new Refused("it sends the balance somewhere other than the sponsor");
        }
        continue;
      }

      throw new Refused("it does something besides handing the account back");
    }

    if (merges !== 1) {
      throw new Refused("it does not hand the account back exactly once");
    }
  }

  // batch: payments only, one asset, one source — and a total this page
  // computed itself.
  //
  // The rules mirror settlement/describe_batch.go, and they exist because a
  // person approving ninety rows reads the number and skims the list. That is
  // what a total is for, so the total has to be true in a stronger sense than a
  // rendered field usually is.
  //
  // The total is returned rather than checked against anything the server sent.
  // A page that displayed the server's number and verified its own quietly
  // would show the wrong one on exactly the day the check was what broke.
  function batch(d, expected) {
    if (!d.operations.length) {
      throw new Refused("it has no operations");
    }
    if (expected && expected.source && d.source !== expected.source) {
      throw new Refused("it is not sent from the account this batch is for");
    }

    var asset = null;
    var total = 0n;

    for (var i = 0; i < d.operations.length; i++) {
      var op = d.operations[i];
      if (op.type !== "payment") {
        // One trustline change among ninety payments is invisible to a reader
        // checking a total, and the total says nothing about it.
        throw new Refused("row " + (i + 1) + " is a " + op.type + ", and a batch is payments only");
      }
      if (op.source && op.source !== d.source) {
        throw new Refused("row " + (i + 1) + " is sent from a different account");
      }

      var rowAsset = field(op, "asset");
      var amount = field(op, "amount");
      var to = field(op, "destination");
      if (!rowAsset || !amount || !to) {
        throw new Refused("row " + (i + 1) + " is missing a field");
      }
      if (asset === null) asset = rowAsset.value;
      if (rowAsset.value !== asset) {
        // A total is only a number when everything in it shares a unit.
        throw new Refused("row " + (i + 1) + " sends a different asset from the rest");
      }

      var stroops = toStroops(amount.value);
      if (stroops <= 0n) {
        throw new Refused("row " + (i + 1) + " sends nothing");
      }
      total += stroops;
    }

    return { total: fromStroops(total), asset: asset, rows: d.operations.length };
  }

  // kind names what a treasury's envelope is, in the page's own judgement.
  //
  // The approval page shows whatever it is sent, operation by operation, and
  // that stays the thing a signer reads. This is the sentence above it: "open a
  // trustline", "swap". It is worked out here, from the envelope, and never
  // taken from the server — a heading the server supplied would be the server
  // describing what it is asking somebody to sign.
  //
  // Each name is given only to exactly the shape it means. A swap is one path
  // payment from the treasury to the treasury; a path payment that delivers to
  // anybody else is a payment to them in another asset, which is a different
  // thing to agree to, and is not called a swap. Anything that fits no shape
  // is "other", and the page says nothing above the operations but that they
  // should be read.
  function kind(d) {
    var ops = d.operations;
    if (!ops.length) return "other";
    for (var i = 0; i < ops.length; i++) {
      if (ops[i].source && ops[i].source !== d.source) return "other";
    }

    if (ops.length > 1) {
      for (var j = 0; j < ops.length; j++) {
        if (ops[j].type !== "payment") return "other";
      }
      return "batch";
    }

    var op = ops[0];
    if (op.type === "payment") return "payment";
    if (op.type === "change_trust") {
      var limit = field(op, "limit");
      if (!limit || !field(op, "asset")) return "other";
      return toStroops(limit.value) === 0n ? "trustline-close" : "trustline-open";
    }
    if (op.type === "path_payment_strict_send") {
      var to = field(op, "destination");
      var floor = field(op, "minimum received");
      if (!to || !floor || to.value !== d.source) return "other";
      // A floor of nothing is no floor: the swap would accept any price.
      if (toStroops(floor.value) <= 0n) return "other";
      return "swap";
    }
    return "other";
  }

  // Amounts go through BigInt, never Number. Ninety rows of seven decimal
  // places is exactly where floating point stops being able to add.
  function toStroops(amount) {
    var parts = String(amount).split(".");
    if (parts.length > 2) throw new Refused("an amount has two decimal points");
    var whole = parts[0] || "0";
    var fraction = (parts[1] || "").padEnd(7, "0");
    if (fraction.length > 7) throw new Refused("an amount has too many decimal places");
    if (!/^\d+$/.test(whole) || !/^\d*$/.test(fraction)) {
      throw new Refused("an amount is not a plain number");
    }
    return BigInt(whole) * 10000000n + BigInt(fraction || "0");
  }

  function fromStroops(stroops) {
    var whole = stroops / 10000000n;
    var fraction = (stroops % 10000000n).toString().padStart(7, "0");
    return whole.toString() + "." + fraction;
  }

  return { reclaim: reclaim, batch: batch, kind: kind, Refused: Refused, fromStroops: fromStroops };
});
