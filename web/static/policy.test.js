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
