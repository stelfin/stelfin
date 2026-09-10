// The shape rules, tested against real envelopes.
//
// Every case here is an envelope that describes itself accurately and is still
// not the thing the page offered to sign. That is the point: the description is
// honest and the transaction is wrong, so a page that only checked the
// description would sign every one of them.
const test = require("node:test");
const assert = require("node:assert");

const StellarSdk = require("./stellar-sdk.min.js");
const describe = require("./describe.js");
const policy = require("./policy.js");

const NETWORK = StellarSdk.Networks.TESTNET;

const account = StellarSdk.Keypair.fromRawEd25519Seed(Buffer.alloc(32, 1));
const sponsor = StellarSdk.Keypair.fromRawEd25519Seed(Buffer.alloc(32, 2));
const stranger = StellarSdk.Keypair.fromRawEd25519Seed(Buffer.alloc(32, 3));
const issuer = StellarSdk.Keypair.fromRawEd25519Seed(Buffer.alloc(32, 4));

const expected = { address: account.publicKey(), destination: sponsor.publicKey() };

function build(operations, source) {
  const acct = new StellarSdk.Account(source || account.publicKey(), "1");
  const b = new StellarSdk.TransactionBuilder(acct, {
    fee: "100",
    networkPassphrase: NETWORK,
  });
  for (const op of operations) b.addOperation(op);
  return describe.describeTx(b.setTimeout(1200).build().toXDR(), NETWORK);
}

const removeTrustline = () =>
  StellarSdk.Operation.changeTrust({
    asset: new StellarSdk.Asset("USDC", issuer.publicKey()),
    limit: "0",
  });

const merge = (to) =>
  StellarSdk.Operation.accountMerge({ destination: to || sponsor.publicKey() });

test("a plain hand-back is accepted", () => {
  policy.reclaim(build([merge()]), expected);
  policy.reclaim(build([removeTrustline(), merge()]), expected);
});

test("a merge to anywhere else is refused", () => {
  // The whole account, to the wrong place, described perfectly accurately.
  assert.throws(
    () => policy.reclaim(build([merge(stranger.publicKey())]), expected),
    /somewhere other than the sponsor/
  );
});

test("an operation the page did not describe is refused", () => {
  const withPayment = build([
    StellarSdk.Operation.payment({
      destination: stranger.publicKey(),
      asset: StellarSdk.Asset.native(),
      amount: "100",
    }),
    merge(),
  ]);
  assert.throws(
    () => policy.reclaim(withPayment, expected),
    /besides handing the account back/
  );
});

test("a trustline that is changed rather than removed is refused", () => {
  // Leaves the reserve locked, so the hand-back releases less than it says.
  const raised = build([
    StellarSdk.Operation.changeTrust({
      asset: new StellarSdk.Asset("USDC", issuer.publicKey()),
      limit: "1000",
    }),
    merge(),
  ]);
  assert.throws(() => policy.reclaim(raised, expected), /instead of removing one/);
});

test("an envelope with no merge is refused", () => {
  assert.throws(
    () => policy.reclaim(build([removeTrustline()]), expected),
    /exactly once/
  );
});

test("an envelope from a different account is refused", () => {
  const elsewhere = build([merge()], stranger.publicKey());
  assert.throws(
    () => policy.reclaim(elsewhere, expected),
    /not sent from the account being handed back/
  );
});

test("an operation sourced by someone else is refused", () => {
  // Valid, describable, and it merges an account nobody on this page agreed to
  // close.
  const mixed = build([
    StellarSdk.Operation.accountMerge({
      destination: sponsor.publicKey(),
      source: stranger.publicKey(),
    }),
  ]);
  assert.throws(
    () => policy.reclaim(mixed, expected),
    /comes from a different account/
  );
});

// ---------------------------------------------------------------------------
// batch
// ---------------------------------------------------------------------------

const usdc = new StellarSdk.Asset("USDC", issuer.publicKey());

function payTo(to, amount, asset) {
  return StellarSdk.Operation.payment({
    destination: to,
    asset: asset || usdc,
    amount: amount,
  });
}

const source = account.publicKey();

test("a batch totals its rows exactly", () => {
  const d = build([
    payTo(sponsor.publicKey(), "250"),
    payTo(stranger.publicKey(), "1000.50"),
    payTo(sponsor.publicKey(), "0.0000001"),
  ]);
  const got = policy.batch(d, { source });
  // 250 + 1000.50 + 0.0000001, added in stroops. Ninety rows of seven decimal
  // places is exactly where floating point stops being able to add.
  assert.equal(got.total, "1250.5000001");
  assert.equal(got.rows, 3);
});

test("a non-payment among the rows is refused", () => {
  const d = build([
    payTo(sponsor.publicKey(), "250"),
    StellarSdk.Operation.changeTrust({ asset: usdc, limit: "1000" }),
  ]);
  assert.throws(() => policy.batch(d, { source }), /payments only/);
});

test("two assets cannot share a total", () => {
  const d = build([
    payTo(sponsor.publicKey(), "250"),
    payTo(sponsor.publicKey(), "250", StellarSdk.Asset.native()),
  ]);
  assert.throws(() => policy.batch(d, { source }), /different asset/);
});

test("a row sent from another account is refused", () => {
  const sneaky = StellarSdk.Operation.payment({
    destination: sponsor.publicKey(),
    asset: usdc,
    amount: "250",
    source: stranger.publicKey(),
  });
  const d = build([payTo(sponsor.publicKey(), "250"), sneaky]);
  assert.throws(() => policy.batch(d, { source }), /different account/);
});

test("a batch from the wrong account is refused", () => {
  const d = build([payTo(sponsor.publicKey(), "250")]);
  assert.throws(
    () => policy.batch(d, { source: stranger.publicKey() }),
    /not sent from the account/
  );
});

test("an empty batch is refused", () => {
  // Reached through the description rather than a builder, since the SDK will
  // not build a transaction with no operations.
  assert.throws(() => policy.batch({ operations: [], source }, { source }), /no operations/);
});

test("the browser's total matches the server's", () => {
  // The cross-language guarantee, at the number that actually gets approved.
  // Amounts chosen so a float would visibly disagree.
  const d = build([
    payTo(sponsor.publicKey(), "0.1"),
    payTo(sponsor.publicKey(), "0.2"),
  ]);
  assert.equal(policy.batch(d, { source }).total, "0.3000000");
});

test("the batch corpus case would catch a float implementation", () => {
  // Guards the guard. Most amounts add up the same either way, so a corpus
  // case chosen carelessly proves nothing about exactness — this asserts that
  // this one still tells the two apart, and fails if somebody edits the
  // amounts to something friendlier.
  const fs = require("node:fs");
  const path = require("node:path");
  const describe = require("./describe.js");

  const file = path.join(
    __dirname, "..", "..", "settlement", "testdata", "describe", "batch_payroll.json"
  );
  const c = JSON.parse(fs.readFileSync(file, "utf8"));
  const d = describe.describeTx(c.xdr, c.network);

  const exact = policy.batch(d, { source: d.source }).total;

  let asNumbers = 0;
  for (const op of d.operations) {
    asNumbers += Number(op.fields.find((f) => f.label === "amount").value);
  }
  assert.notEqual(
    asNumbers.toFixed(7),
    exact,
    "these amounts add up the same in floating point, so the corpus case is not testing exactness"
  );
});
