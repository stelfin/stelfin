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

  return { reclaim: reclaim, Refused: Refused };
});
