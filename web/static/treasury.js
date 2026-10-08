// What a call to a treasury contract is, read out of the call.
//
// A contract call arrives as a contract, a function name and some typed
// values, and describe.js shows exactly that: `vote`, `addr:G…`, `u32:3`,
// `bool:true`. That is honest and it is not something a person can agree to.
// This turns it into a sentence — "vote for proposal 3" — and does so from the
// values in the envelope and from nothing else.
//
// Two things make that safe to do here when the general renderer refuses to.
// First, this page is for one contract whose functions are known: the table
// below is that contract's interface, pinned in this file, and a call that is
// not in it, or has one argument more or less, or a value of the wrong type,
// is not read at all. It is refused. Second, nothing here is a guess about
// meaning: `vote(voter, id, approve)` with `bool:true` IS a vote for, by the
// contract's own source, which is in the same repository as this file and is
// checked against it byte for byte in treasury.test.js.
//
// What this cannot read is anything that is not in the envelope. A vote names a
// proposal by its number, and what proposal 3 does is on the ledger. The page
// shows what stelfin says about that, apart, and says whose word it is.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory(require("./stellar-sdk.min.js"));
  } else {
    root.StelfinTreasury = factory(root.StellarSdk);
  }
})(typeof self !== "undefined" ? self : this, function (StellarSdk) {
  "use strict";

  function Refused(message) {
    this.name = "Refused";
    this.message = message;
  }
  Refused.prototype = Object.create(Error.prototype);

  // parse reads one value as scval.js writes it: a type, a colon, and a body.
  //
  // The delimiters that give lists and structs their shape are escaped inside
  // every value that could contain them, so a body that is not a list or a
  // struct runs to the next delimiter and no further. Anything left over
  // after the value is a value that was not what it claimed to be.
  function parse(text) {
    const out = value(String(text), 0);
    if (out.end !== text.length) throw new Refused("a value in it could not be read");
    return out.node;
  }

  function value(text, at) {
    const colon = text.indexOf(":", at);
    if (colon === -1) throw new Refused("a value in it could not be read");
    const type = text.slice(at, colon);
    if (!/^[a-z0-9]+$/.test(type)) throw new Refused("a value in it could not be read");
    let i = colon + 1;

    if (type === "vec") {
      if (text[i] !== "[") throw new Refused("a list in it could not be read");
      i++;
      const items = [];
      while (text[i] !== "]") {
        if (items.length) {
          if (text[i] !== ",") throw new Refused("a list in it could not be read");
          i++;
        }
        const item = value(text, i);
        items.push(item.node);
        i = item.end;
        if (i >= text.length) throw new Refused("a list in it could not be read");
      }
      return { node: { type, items }, end: i + 1 };
    }

    if (type === "map") {
      if (text[i] !== "{") throw new Refused("a struct in it could not be read");
      i++;
      const entries = [];
      while (text[i] !== "}") {
        if (entries.length) {
          if (text[i] !== ",") throw new Refused("a struct in it could not be read");
          i++;
        }
        const key = value(text, i);
        if (text[key.end] !== "=") throw new Refused("a struct in it could not be read");
        const val = value(text, key.end + 1);
        entries.push([key.node, val.node]);
        i = val.end;
        if (i >= text.length) throw new Refused("a struct in it could not be read");
      }
      return { node: { type, entries }, end: i + 1 };
    }

    let end = i;
    while (end < text.length && !",[]{}=".includes(text[end])) end++;
    return { node: { type, value: text.slice(i, end) }, end };
  }

  // The typed readers. Each takes a parsed value and returns it only if it is
  // exactly the type asked for.
  function as(node, type, what) {
    if (!node || node.type !== type || typeof node.value !== "string") {
      throw new Refused(what + " is not the kind of value this contract takes");
    }
    return node.value;
  }

  const address = (node, what) => {
    const text = as(node, "addr", what);
    if (!/^[GC][A-Z2-7]{55}$/.test(text)) throw new Refused(what + " is not an address");
    return text;
  };
  const whole = (node, type, what) => {
    const text = as(node, type, what);
    if (!/^(0|[1-9][0-9]*)$/.test(text)) throw new Refused(what + " is not a whole number");
    return BigInt(text);
  };
  const amount = (node, what) => {
    const text = as(node, "i128", what);
    if (!/^-?(0|[1-9][0-9]*)$/.test(text)) throw new Refused(what + " is not a number");
    return BigInt(text);
  };
  const flag = (node, what) => {
    const text = as(node, "bool", what);
    if (text !== "true" && text !== "false") throw new Refused(what + " is neither yes nor no");
    return text === "true";
  };

  // A struct, with exactly the fields named and no others. The contract's
  // structs travel as maps from field name to value.
  function struct(node, names, what) {
    if (!node || node.type !== "map" || node.entries.length !== names.length) {
      throw new Refused(what + " is not the shape this contract takes");
    }
    const out = {};
    for (const [key, val] of node.entries) {
      if (key.type !== "sym" || !names.includes(key.value) || key.value in out) {
        throw new Refused(what + " has a field this contract does not");
      }
      out[key.value] = val;
    }
    return out;
  }

  // An enum, as its name and the one thing it carries.
  function variant(node, what) {
    if (!node || node.type !== "vec" || node.items.length !== 2 || node.items[0].type !== "sym") {
      throw new Refused(what + " is not the shape this contract takes");
    }
    return { name: node.items[0].value, payload: node.items[1] };
  }

  // Amounts of a classic asset count in ten-millionths. Through BigInt, never
  // Number: an i128 does not fit in one, and a payment is the last place to
  // round.
  function units(stroops) {
    const negative = stroops < 0n;
    const n = negative ? -stroops : stroops;
    const wholePart = (n / 10000000n).toString();
    let fraction = (n % 10000000n).toString().padStart(7, "0").replace(/0+$/, "");
    if (fraction.length < 2) fraction = fraction.padEnd(2, "0");
    return (negative ? "-" : "") + wholePart.replace(/\B(?=(\d{3})+(?!\d))/g, ",") + "." + fraction;
  }

  // An amount of a token, as much as this page knows about the token.
  //
  // A token this page has worked out the name of is a classic asset, and those
  // count in ten-millionths. One it has not is a contract it knows nothing
  // about, including how it counts — so the number is shown as the contract
  // will receive it, and said to be that.
  function tokenAmount(stroops, token, tokens) {
    const name = tokens && tokens[token];
    if (name) return units(stroops) + " " + name;
    return stroops.toString() + " of that token's smallest unit";
  }

  const tokenName = (token, tokens) => (tokens && tokens[token] ? tokens[token] : "a token this page cannot name");

  function span(seconds) {
    if (seconds === 0n) return "none";
    const parts = [];
    for (const [size, name] of [[86400n, "d"], [3600n, "h"], [60n, "m"], [1n, "s"]]) {
      const n = seconds / size;
      if (n > 0n) {
        parts.push(n.toString() + name);
        seconds %= size;
      }
    }
    return parts.join(" ");
  }

  function percent(bps) {
    const wholePart = bps / 100n;
    const fraction = bps % 100n;
    return fraction === 0n ? wholePart + "%" : wholePart + "." + fraction.toString().padStart(2, "0") + "%";
  }

  function policyRows(node) {
    const f = struct(
      node,
      ["approval_bps", "quorum_bps", "spend_limit", "spend_window", "timelock", "voting_period"],
      "the policy"
    );
    const limit = amount(f.spend_limit, "the spending allowance");
    return [
      { label: "Quorum", value: percent(whole(f.quorum_bps, "u32", "the quorum")) + " of all voting weight must vote" },
      { label: "Approval", value: percent(whole(f.approval_bps, "u32", "the approval")) + " of those voting must agree" },
      { label: "Voting stays open", value: span(whole(f.voting_period, "u64", "the voting period")) },
      { label: "Timelock", value: span(whole(f.timelock, "u64", "the timelock")) },
      {
        label: "Spending without a proposal",
        value:
          limit === 0n
            ? "none"
            : "up to " + units(limit) + " every " + span(whole(f.spend_window, "u64", "the spending window")),
      },
    ];
  }

  // What a proposal does, from the action it carries.
  function action(node, tokens) {
    const v = variant(node, "what is proposed");
    switch (v.name) {
      case "Pay": {
        const f = struct(v.payload, ["amount", "to", "token"], "the payment");
        const sum = amount(f.amount, "the amount");
        const token = address(f.token, "the token");
        if (sum <= 0n) throw new Refused("it proposes paying nothing");
        return {
          heading: "Propose a payment",
          what: "It asks the treasury's voters to pay this out. Nothing is paid unless they pass it and its timelock runs.",
          rows: [
            { label: "Pay", value: tokenAmount(sum, token, tokens) },
            { label: "Token", value: token, note: tokenName(token, tokens), code: true },
            { label: "To", value: address(f.to, "the recipient"), code: true },
          ],
        };
      }
      case "SetPolicy":
        return {
          heading: "Propose new rules",
          what: "It asks the treasury's voters to replace its rules with these. Nothing changes unless they pass it and its timelock runs.",
          rows: policyRows(v.payload),
        };
      case "SetMember": {
        const f = struct(v.payload, ["address", "weight"], "the voter");
        const weight = whole(f.weight, "u32", "the weight");
        const who = address(f.address, "the voter");
        if (weight === 0n) {
          return {
            heading: "Propose removing a voter",
            what: "It asks the treasury's voters to take this address's vote away. Nothing changes unless they pass it and its timelock runs.",
            rows: [{ label: "Voter", value: who, code: true }],
          };
        }
        return {
          heading: "Propose a voter",
          what: "It asks the treasury's voters to give this address a vote of this weight. Nothing changes unless they pass it and its timelock runs.",
          rows: [
            { label: "Voter", value: who, code: true },
            { label: "Weight", value: weight.toString() },
          ],
        };
      }
      case "AllowRecipient":
        return {
          heading: "Propose a new recipient",
          what: "It asks the treasury's voters to let the treasury pay this address. Nothing changes unless they pass it and its timelock runs.",
          rows: [{ label: "Recipient", value: address(v.payload, "the recipient"), code: true }],
        };
      case "DenyRecipient":
        return {
          heading: "Propose removing a recipient",
          what: "It asks the treasury's voters to stop the treasury paying this address. It takes effect as soon as they pass it, with no timelock: it can only stop something.",
          rows: [{ label: "Recipient", value: address(v.payload, "the recipient"), code: true }],
        };
      case "Revoke": {
        const target = whole(v.payload, "u32", "the proposal");
        return {
          heading: "Propose stopping proposal " + target,
          what: "It asks the treasury's voters to stop that proposal before it is carried out. It takes effect as soon as they pass it, with no timelock: it can only stop something.",
          rows: [{ label: "Proposal to stop", value: target.toString() }],
        };
      }
      default:
        throw new Refused("it proposes something this page does not know how to read");
    }
  }

  // The treasury's own functions that a member calls, and what each takes.
  // This is the contract's interface: contracts/dao-treasury/src/lib.rs.
  const CALLS = {
    propose(args, d, tokens) {
      if (args.length !== 3) throw new Refused("propose takes three values and this has " + args.length);
      mine(args[0], d, "the proposer");
      const memo = as(args[2], "bytes", "the commitment to its note");
      if (!/^[0-9a-f]{64}$/.test(memo)) throw new Refused("its note's commitment is not 32 bytes");
      const out = action(args[1], tokens);
      out.kind = "propose";
      out.memo = memo;
      return out;
    },
    vote(args, d) {
      if (args.length !== 3) throw new Refused("vote takes three values and this has " + args.length);
      mine(args[0], d, "the voter");
      const id = whole(args[1], "u32", "the proposal");
      const approve = flag(args[2], "the vote");
      return {
        kind: "vote",
        heading: "Vote " + (approve ? "for" : "against") + " proposal " + id,
        what: "The treasury's contract counts it with whatever weight it gives your wallet. A vote is cast once and cannot be changed.",
        rows: [
          { label: "Proposal", value: id.toString() },
          { label: "Your vote", value: approve ? "For" : "Against" },
        ],
      };
    },
    cancel(args, d) {
      if (args.length !== 2) throw new Refused("cancel takes two values and this has " + args.length);
      mine(args[0], d, "the proposer");
      const id = whole(args[1], "u32", "the proposal");
      return {
        kind: "cancel",
        heading: "Withdraw proposal " + id,
        what: "The contract lets whoever made a proposal withdraw it while it is still open. Nothing it proposed will happen.",
        rows: [{ label: "Proposal", value: id.toString() }],
      };
    },
    spend(args, d, tokens) {
      if (args.length !== 4) throw new Refused("spend takes four values and this has " + args.length);
      mine(args[0], d, "the member spending");
      const token = address(args[1], "the token");
      const sum = amount(args[3], "the amount");
      if (sum <= 0n) throw new Refused("it spends nothing");
      return {
        kind: "spend",
        heading: "Spend from the treasury",
        what: "It pays out of the treasury now, on your signature alone. The contract allows that only up to the limit its voters set, and only to an address on its list.",
        rows: [
          { label: "Pay", value: tokenAmount(sum, token, tokens) },
          { label: "Token", value: token, note: tokenName(token, tokens), code: true },
          { label: "To", value: address(args[2], "the recipient"), code: true },
        ],
      };
    },
  };

  // The address a call is made in the name of has to be the account making
  // it. Otherwise the call is one member's transaction claiming to act for
  // another, which the contract would refuse and which nobody should sign.
  function mine(node, d, what) {
    if (address(node, what) !== d.source) {
      throw new Refused(what + " it names is not the wallet it is sent from");
    }
  }

  function field(op, label) {
    const f = op.fields.find((x) => x.label === label);
    if (!f) throw new Refused("it is missing its " + label);
    return f.value;
  }

  // read turns a described envelope into what it asks, or refuses.
  //
  // expected names the treasury this link was issued for and the wallet it was
  // issued to. Both come from the server and both are checked against the
  // envelope here, so what they do is narrow what the page will read: a call
  // to some other contract, or from some other account, is not the thing this
  // link is for, however accurately it describes itself.
  function read(d, expected) {
    if (d.operations.length !== 1) {
      throw new Refused("it does " + d.operations.length + " things, and a call to a treasury is one");
    }
    const op = d.operations[0];
    if (op.type !== "invoke_contract") throw new Refused("it is not a call to a contract");
    if (op.source && op.source !== d.source) throw new Refused("its operation comes from a different account");
    if (d.source !== expected.member) {
      throw new Refused("it is not sent from the wallet this link was made for");
    }
    if (d.memo_type !== "none") throw new Refused("it carries a memo, and a call to a treasury has none");

    // Who is being asked to authorise it. Only the account sending it: an
    // entry in anybody else's name is that somebody's authority, riding along.
    const authorizations = Number(field(op, "authorizations"));
    if (!(authorizations === 0 || authorizations === 1)) {
      throw new Refused("it asks for more than your own authority");
    }
    for (let i = 0; i < authorizations; i++) {
      if (field(op, "authorized by " + i) !== "the transaction's source account") {
        throw new Refused("it asks somebody other than you to authorise something");
      }
    }

    const contract = field(op, "contract");
    const name = field(op, "function");
    const count = Number(field(op, "arguments"));
    const args = [];
    for (let i = 0; i < count; i++) args.push(parse(field(op, "arg " + i)));
    const tokens = expected.tokens || {};

    let out;
    if (contract === expected.treasury) {
      const call = Object.prototype.hasOwnProperty.call(CALLS, name) ? CALLS[name] : null;
      if (!call) throw new Refused("it calls " + name + ", which is not something a member does to a treasury here");
      out = call(args, d, tokens);
    } else if (name === "transfer") {
      // The one call that is not to the treasury: moving your own money into
      // it, which is done through the token. From you, to the treasury, and
      // nowhere else.
      if (args.length !== 3) throw new Refused("a transfer takes three values and this has " + args.length);
      mine(args[0], d, "the sender");
      if (address(args[1], "the destination") !== expected.treasury) {
        throw new Refused("it moves your money somewhere other than this treasury");
      }
      const sum = amount(args[2], "the amount");
      if (sum <= 0n) throw new Refused("it moves nothing");
      out = {
        kind: "deposit",
        heading: "Deposit into the treasury",
        what: "It moves your own money into the treasury. From then on it leaves only by the voters' decision.",
        rows: [
          { label: "Deposit", value: tokenAmount(sum, contract, tokens) },
          { label: "Token", value: contract, note: tokenName(contract, tokens), code: true },
          { label: "From", value: d.source, note: "your wallet", code: true },
        ],
      };
    } else {
      throw new Refused("it calls a contract that is not this treasury");
    }

    out.treasury = expected.treasury;
    out.function = name;
    return out;
  }

  // tokens works out the contract of each classic asset it is told about.
  //
  // The server says which assets a token might be; it does not say which
  // contract belongs to which. That is arithmetic on the asset and the network,
  // done here, so a name is only ever put to a contract this page derived.
  function tokens(assets, passphrase) {
    const out = {};
    for (const hint of assets || []) {
      try {
        const asset =
          hint.issuer === "" || hint.issuer === undefined
            ? StellarSdk.Asset.native()
            : new StellarSdk.Asset(hint.code, hint.issuer);
        const name = asset.isNative() ? "XLM" : asset.getCode() + " (issued by " + asset.getIssuer() + ")";
        out[asset.contractId(passphrase)] = name;
      } catch (_) {
        // Not an asset. It names nothing, which is the safe way to be wrong.
      }
    }
    return out;
  }

  // The commitment a proposal carries to its proposer's note, as hex.
  //
  // Must match noteMemo in api/daotreasury.go. The page computes this from the
  // note the server shows and compares it with what the envelope carries, so
  // a note is only shown as the proposal's own when it is.
  const NOTE_DOMAIN = "stelfin proposal note v1\n";

  async function noteCommitment(note, subtle) {
    const bytes = new TextEncoder().encode(NOTE_DOMAIN + String(note || ""));
    const digest = await (subtle || crypto.subtle).digest("SHA-256", bytes);
    let out = "";
    for (const b of new Uint8Array(digest)) out += b.toString(16).padStart(2, "0");
    return out;
  }

  return { read, parse, tokens, noteCommitment, units, Refused, NOTE_DOMAIN };
});
