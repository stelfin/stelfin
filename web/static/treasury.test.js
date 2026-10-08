// What the page says a treasury call does, against real envelopes.
//
// The values come from the contract. contracts/dao-treasury/testdata/abi.tsv is
// written by the contract's own test suite from its real types, and the Go
// that builds these calls is checked against the same file — so the three
// programs that have to agree about what `Action::Pay` looks like on the wire
// each fail on their own when one of them stops agreeing.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const { webcrypto } = require("node:crypto");

const StellarSdk = require("./stellar-sdk.min.js");
const describe = require("./describe.js");
const treasury = require("./treasury.js");

const NETWORK = StellarSdk.Networks.TESTNET;
const { xdr, nativeToScVal, Address } = StellarSdk;

const me = StellarSdk.Keypair.fromRawEd25519Seed(Buffer.alloc(32, 1));
const someoneElse = StellarSdk.Keypair.fromRawEd25519Seed(Buffer.alloc(32, 2));
const TREASURY = StellarSdk.StrKey.encodeContract(Buffer.alloc(32, 9));
const ELSEWHERE = StellarSdk.StrKey.encodeContract(Buffer.alloc(32, 8));

// The same addresses the contract's suite encodes.
const PAYEE = "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5";
const USDC = "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA";
const XLM = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC";

const golden = Object.fromEntries(
  fs
    .readFileSync(path.join(__dirname, "..", "..", "contracts", "dao-treasury", "testdata", "abi.tsv"), "utf8")
    .trim()
    .split("\n")
    .map((line) => {
      const [name, hex] = line.split("\t");
      return [name, xdr.ScVal.fromXDR(hex, "hex")];
    })
);

const addr = (a) => new Address(a).toScVal();
const u32 = (n) => nativeToScVal(n, { type: "u32" });
const i128 = (n) => nativeToScVal(n, { type: "i128" });
const memo = () => xdr.ScVal.scvBytes(Buffer.alloc(32, 7));

function envelope(operations, options = {}) {
  const account = new StellarSdk.Account(options.source || me.publicKey(), "1");
  const b = new StellarSdk.TransactionBuilder(account, { fee: "100", networkPassphrase: NETWORK });
  for (const op of operations) b.addOperation(op);
  if (options.memo) b.addMemo(options.memo);
  return describe.describeTx(b.setTimeout(600).build().toXDR(), NETWORK);
}

const invoke = (contract, name, args, auth) =>
  StellarSdk.Operation.invokeContractFunction({ contract, function: name, args, auth });

const call = (name, args, options) => envelope([invoke(TREASURY, name, args)], options);

const tokens = treasury.tokens(
  [
    { code: "XLM", issuer: "" },
    { code: "USDC", issuer: PAYEE },
  ],
  NETWORK
);
const expected = { treasury: TREASURY, member: me.publicKey(), tokens };

const read = (d, overrides) => treasury.read(d, { ...expected, ...overrides });
const rows = (r) => Object.fromEntries(r.rows.map((row) => [row.label, row.value]));

test("a token is named only by the contract this page derives for it", () => {
  // The same two addresses the Go side derives, in settlement's tests: the
  // network's own asset, and Circle's testnet USDC.
  assert.strictEqual(tokens[XLM], "XLM");
  assert.strictEqual(tokens[USDC], "USDC (issued by " + PAYEE + ")");
  assert.strictEqual(Object.keys(tokens).length, 2);

  // An asset that is not one names nothing.
  assert.deepStrictEqual(treasury.tokens([{ code: "not an asset!", issuer: "G" }], NETWORK), {});
  // And the same asset on another network is a different contract.
  const elsewhere = treasury.tokens([{ code: "XLM", issuer: "" }], StellarSdk.Networks.PUBLIC);
  assert.ok(!(XLM in elsewhere));
});

test("a vote is read as a vote, each way", () => {
  const yes = read(call("vote", [addr(me.publicKey()), u32(3), xdr.ScVal.scvBool(true)]));
  assert.strictEqual(yes.kind, "vote");
  assert.strictEqual(yes.heading, "Vote for proposal 3");
  assert.deepStrictEqual(rows(yes), { Proposal: "3", "Your vote": "For" });

  const no = read(call("vote", [addr(me.publicKey()), u32(3), xdr.ScVal.scvBool(false)]));
  assert.strictEqual(no.heading, "Vote against proposal 3");
  assert.deepStrictEqual(rows(no), { Proposal: "3", "Your vote": "Against" });
});

test("each thing the contract can be asked to do is read from the contract's own encoding", () => {
  const propose = (action) => read(call("propose", [addr(me.publicKey()), golden[action], memo()]));

  const pay = propose("action_pay");
  assert.strictEqual(pay.kind, "propose");
  assert.strictEqual(pay.heading, "Propose a payment");
  assert.strictEqual(pay.memo, "07".repeat(32));
  assert.deepStrictEqual(rows(pay), {
    Pay: "1,250.00 USDC (issued by " + PAYEE + ")",
    Token: USDC,
    To: PAYEE,
  });

  const rules = propose("action_set_policy");
  assert.strictEqual(rules.heading, "Propose new rules");
  assert.deepStrictEqual(rows(rules), {
    Quorum: "50% of all voting weight must vote",
    Approval: "60% of those voting must agree",
    "Voting stays open": "3d",
    Timelock: "1d",
    "Spending without a proposal": "up to 250.00 every 1d",
  });

  const removal = propose("action_set_member");
  assert.strictEqual(removal.heading, "Propose removing a voter");
  assert.deepStrictEqual(rows(removal), { Voter: PAYEE });

  assert.strictEqual(propose("action_allow_recipient").heading, "Propose a new recipient");
  const deny = propose("action_deny_recipient");
  assert.strictEqual(deny.heading, "Propose removing a recipient");
  assert.match(deny.what, /no timelock/);

  const stop = propose("action_revoke");
  assert.strictEqual(stop.heading, "Propose stopping proposal 7");
  assert.match(stop.what, /no timelock/);

  // Every action in the contract's file is one of the six read above. One
  // added there and not here fails rather than reaching a page that would
  // refuse it in front of somebody.
  const actions = Object.keys(golden).filter((name) => name.startsWith("action_"));
  assert.deepStrictEqual(actions.sort(), [
    "action_allow_recipient",
    "action_deny_recipient",
    "action_pay",
    "action_revoke",
    "action_set_member",
    "action_set_policy",
  ]);
});

test("a voter given weight is read as that, and a token this page cannot name is not named", () => {
  const member = xdr.ScVal.scvMap([
    new xdr.ScMapEntry({ key: xdr.ScVal.scvSymbol("address"), val: addr(PAYEE) }),
    new xdr.ScMapEntry({ key: xdr.ScVal.scvSymbol("weight"), val: u32(2) }),
  ]);
  const voter = read(
    call("propose", [addr(me.publicKey()), xdr.ScVal.scvVec([xdr.ScVal.scvSymbol("SetMember"), member]), memo()])
  );
  assert.strictEqual(voter.heading, "Propose a voter");
  assert.deepStrictEqual(rows(voter), { Voter: PAYEE, Weight: "2" });

  // The same payment, read by a page that was told about no assets: the
  // amount is what the contract will receive, and is said to be that.
  const pay = read(call("propose", [addr(me.publicKey()), golden.action_pay, memo()]), { tokens: {} });
  assert.strictEqual(rows(pay).Pay, "12500000000 of that token's smallest unit");
  assert.strictEqual(pay.rows[1].note, "a token this page cannot name");
});

test("withdrawing, spending and depositing are each read as what they are", () => {
  const cancel = read(call("cancel", [addr(me.publicKey()), u32(4)]));
  assert.strictEqual(cancel.heading, "Withdraw proposal 4");

  const spend = read(call("spend", [addr(me.publicKey()), addr(XLM), addr(PAYEE), i128(205000000n)]));
  assert.strictEqual(spend.kind, "spend");
  assert.deepStrictEqual(rows(spend), { Pay: "20.50 XLM", Token: XLM, To: PAYEE });
  assert.match(spend.what, /on your signature alone/);

  const deposit = read(envelope([invoke(XLM, "transfer", [addr(me.publicKey()), addr(TREASURY), i128(1n)])]));
  assert.strictEqual(deposit.kind, "deposit");
  assert.deepStrictEqual(rows(deposit), { Deposit: "0.0000001 XLM", Token: XLM, From: me.publicKey() });
});

test("a call that is not the one this link is for is refused", () => {
  const vote = [addr(me.publicKey()), u32(3), xdr.ScVal.scvBool(true)];
  const refused = (d, pattern, overrides) => assert.throws(() => read(d, overrides), pattern);

  // Described perfectly accurately, every one of them.
  refused(envelope([invoke(ELSEWHERE, "vote", vote)]), /not this treasury/);
  refused(call("vote", vote, { source: someoneElse.publicKey() }), /not sent from the wallet this link was made for/);
  refused(call("vote", [addr(someoneElse.publicKey()), u32(3), xdr.ScVal.scvBool(true)]), /not the wallet it is sent from/);
  refused(call("vote", vote), /not sent from the wallet/, { member: someoneElse.publicKey() });
  refused(envelope([invoke(TREASURY, "vote", vote), invoke(TREASURY, "vote", vote)]), /does 2 things/);
  refused(call("vote", vote, { memo: StellarSdk.Memo.text("hi") }), /carries a memo/);
  refused(
    envelope([StellarSdk.Operation.payment({ destination: PAYEE, asset: StellarSdk.Asset.native(), amount: "1" })]),
    /not a call to a contract/
  );
});

test("a function a member does not call is refused, whatever it is", () => {
  for (const name of ["execute", "tally", "bump", "set_member", "init", "upgrade", "__constructor", "transfer"]) {
    assert.throws(
      () => read(call(name, [addr(me.publicKey()), u32(1)])),
      /not something a member does to a treasury here/,
      name
    );
  }
  // Names every object has are not functions of the contract either.
  for (const name of ["constructor", "toString", "hasOwnProperty"]) {
    assert.throws(() => read(call(name, [])), /not something a member does/, name);
  }
});

test("a call of the wrong shape is refused, not read as far as it goes", () => {
  const who = addr(me.publicKey());
  const cases = [
    [call("vote", [who, u32(3)]), /takes three values/],
    [call("vote", [who, u32(3), xdr.ScVal.scvBool(true), u32(9)]), /takes three values/],
    // The right digits at the wrong width are a different value.
    [call("vote", [who, nativeToScVal(3, { type: "u64" }), xdr.ScVal.scvBool(true)]), /not the kind of value/],
    [call("vote", [who, u32(3), u32(1)]), /not the kind of value/],
    [call("cancel", [who]), /takes two values/],
    [call("spend", [who, addr(XLM), addr(PAYEE), i128(0n)]), /spends nothing/],
    [call("spend", [who, addr(XLM), addr(PAYEE), i128(-5n)]), /spends nothing/],
    [call("propose", [who, golden.action_pay]), /takes three values/],
    [call("propose", [who, golden.action_pay, xdr.ScVal.scvBytes(Buffer.alloc(31, 7))]), /not 32 bytes/],
    [call("propose", [who, golden.policy, memo()]), /not the shape/],
    [
      call("propose", [who, xdr.ScVal.scvVec([xdr.ScVal.scvSymbol("Sweep"), addr(PAYEE)]), memo()]),
      /does not know how to read/,
    ],
    // A payment of nothing, and a policy with a field too few.
    [
      call("propose", [
        who,
        xdr.ScVal.scvVec([
          xdr.ScVal.scvSymbol("Pay"),
          xdr.ScVal.scvMap([
            new xdr.ScMapEntry({ key: xdr.ScVal.scvSymbol("amount"), val: i128(0n) }),
            new xdr.ScMapEntry({ key: xdr.ScVal.scvSymbol("to"), val: addr(PAYEE) }),
            new xdr.ScMapEntry({ key: xdr.ScVal.scvSymbol("token"), val: addr(XLM) }),
          ]),
        ]),
        memo(),
      ]),
      /paying nothing/,
    ],
    [
      call("propose", [
        who,
        xdr.ScVal.scvVec([
          xdr.ScVal.scvSymbol("SetMember"),
          xdr.ScVal.scvMap([new xdr.ScMapEntry({ key: xdr.ScVal.scvSymbol("address"), val: addr(PAYEE) })]),
        ]),
        memo(),
      ]),
      /not the shape/,
    ],
  ];
  for (const [d, pattern] of cases) assert.throws(() => read(d), pattern);
});

test("a deposit goes from you to this treasury, or it is not a deposit", () => {
  const who = addr(me.publicKey());
  const transfer = (args) => envelope([invoke(XLM, "transfer", args)]);

  assert.throws(() => read(transfer([who, addr(ELSEWHERE), i128(5n)])), /somewhere other than this treasury/);
  assert.throws(() => read(transfer([who, addr(PAYEE), i128(5n)])), /somewhere other than this treasury/);
  assert.throws(() => read(transfer([addr(PAYEE), addr(TREASURY), i128(5n)])), /not the wallet it is sent from/);
  assert.throws(() => read(transfer([who, addr(TREASURY), i128(0n)])), /moves nothing/);
  assert.throws(() => read(transfer([who, addr(TREASURY)])), /takes three values/);
  // Any other function of a token is not a deposit, and is no call this page
  // is for: `approve` is how a token is told to let somebody else spend.
  assert.throws(
    () => read(envelope([invoke(XLM, "approve", [who, addr(ELSEWHERE), i128(5n), u32(100)])])),
    /not this treasury/
  );
});

test("an authorisation in anybody else's name is refused", () => {
  const args = [addr(me.publicKey()), u32(3), xdr.ScVal.scvBool(true)];
  const invocation = new xdr.SorobanAuthorizedInvocation({
    function: xdr.SorobanAuthorizedFunction.sorobanAuthorizedFunctionTypeContractFn(
      new xdr.InvokeContractArgs({
        contractAddress: new Address(TREASURY).toScAddress(),
        functionName: "vote",
        args,
      })
    ),
    subInvocations: [],
  });

  // The account sending it authorising its own call: what simulation records,
  // and fine.
  const own = new xdr.SorobanAuthorizationEntry({
    credentials: xdr.SorobanCredentials.sorobanCredentialsSourceAccount(),
    rootInvocation: invocation,
  });
  assert.strictEqual(read(envelope([invoke(TREASURY, "vote", args, [own])])).kind, "vote");

  // Somebody else's authority, riding along in an envelope you would sign.
  const theirs = new xdr.SorobanAuthorizationEntry({
    credentials: xdr.SorobanCredentials.sorobanCredentialsAddress(
      new xdr.SorobanAddressCredentials({
        address: new Address(someoneElse.publicKey()).toScAddress(),
        nonce: xdr.Int64.fromString("1"),
        signatureExpirationLedger: 100,
        signature: xdr.ScVal.scvVoid(),
      })
    ),
    rootInvocation: invocation,
  });
  assert.throws(
    () => read(envelope([invoke(TREASURY, "vote", args, [theirs])])),
    /somebody other than you/
  );
  assert.throws(
    () => read(envelope([invoke(TREASURY, "vote", args, [own, own])])),
    /more than your own authority/
  );
});

test("a value is parsed exactly, and a malformed one is refused", () => {
  assert.deepStrictEqual(treasury.parse("u32:7"), { type: "u32", value: "7" });
  assert.deepStrictEqual(treasury.parse("vec:[]"), { type: "vec", items: [] });
  assert.deepStrictEqual(treasury.parse("map:{}"), { type: "map", entries: [] });
  assert.deepStrictEqual(treasury.parse("vec:[sym:Revoke,u32:7]"), {
    type: "vec",
    items: [
      { type: "sym", value: "Revoke" },
      { type: "u32", value: "7" },
    ],
  });
  assert.deepStrictEqual(treasury.parse("map:{sym:a=vec:[u32:1],sym:b=void:}").entries.length, 2);

  // A string that contains the delimiters arrives escaped, so it cannot close
  // the list it is in and smuggle a second value after itself.
  const tricky = describe.describeTx(
    new StellarSdk.TransactionBuilder(new StellarSdk.Account(me.publicKey(), "1"), {
      fee: "100",
      networkPassphrase: NETWORK,
    })
      .addOperation(invoke(TREASURY, "x", [xdr.ScVal.scvVec([xdr.ScVal.scvString("a],u32:9,[b")])]))
      .setTimeout(60)
      .build()
      .toXDR(),
    NETWORK
  );
  const parsed = treasury.parse(tricky.operations[0].fields.find((f) => f.label === "arg 0").value);
  assert.strictEqual(parsed.items.length, 1);
  assert.strictEqual(parsed.items[0].type, "str");

  for (const bad of ["", "u32", "u32:7,u32:8", "vec:[u32:7", "vec:u32:7]", "map:{sym:a}", "map:{sym:a=u32:1", "U32:7", "vec:[u32:7]]"]) {
    assert.throws(() => treasury.parse(bad), /could not be read/, JSON.stringify(bad));
  }
});

test("amounts are exact at every size", () => {
  assert.strictEqual(treasury.units(0n), "0.00");
  assert.strictEqual(treasury.units(1n), "0.0000001");
  assert.strictEqual(treasury.units(10000000n), "1.00");
  assert.strictEqual(treasury.units(12345678900n), "1,234.56789");
  // Past what a Number can hold without rounding.
  assert.strictEqual(treasury.units(170141183460469231731687303715884105727n), "17,014,118,346,046,923,173,168,730,371,588.4105727");
});

test("a note's commitment is the one the server computes", async () => {
  // The same two values api/daotreasury_test.go pins, so the page and the
  // server cannot come to disagree about what a note hashes to.
  assert.strictEqual(
    await treasury.noteCommitment("March invoice", webcrypto.subtle),
    "41fc38841e5f22812dcac1848a468b9ab1bd9c51c62712091f3b746d02a8488b"
  );
  assert.strictEqual(
    await treasury.noteCommitment("", webcrypto.subtle),
    "b40d4ebb204d39ea6ddcddfb44424ae37c16fa644ed545d8ba6140d3dea49085"
  );
  assert.strictEqual(
    await treasury.noteCommitment(undefined, webcrypto.subtle),
    await treasury.noteCommitment("", webcrypto.subtle)
  );
});
