// What a wallet returns is held to being this transaction plus one signature.
const test = require("node:test");
const assert = require("node:assert");
const StellarSdk = require("./stellar-sdk.min.js");
const chain = require("./chain.js");
const make = require("./wallet.js");

const PASSPHRASE = StellarSdk.Networks.TESTNET;
const PUBLIC = StellarSdk.Networks.PUBLIC;

function payment(source, amount = "5") {
  const account = new StellarSdk.Account(source.publicKey(), "100");
  return new StellarSdk.TransactionBuilder(account, { fee: "100", networkPassphrase: PASSPHRASE })
    .addOperation(StellarSdk.Operation.payment({
      destination: StellarSdk.Keypair.random().publicKey(),
      asset: StellarSdk.Asset.native(), amount,
    }))
    .setTimeout(300)
    .build();
}
const signedBy = (tx, ...keys) => {
  const copy = StellarSdk.TransactionBuilder.fromXDR(tx.toXDR(), PASSPHRASE);
  for (const k of keys) copy.sign(k);
  return copy.toXDR();
};

// A Freighter that signs with the given key, on the given network.
function freighterWith(key, { passphrase = PASSPHRASE, connected = true, decline = false, answer } = {}) {
  const calls = [];
  return {
    calls,
    isConnected: async () => ({ isConnected: connected }),
    requestAccess: async () => ({ address: key.publicKey() }),
    getNetworkDetails: async () => ({ network: "X", networkPassphrase: passphrase }),
    signTransaction: async (xdr, opts) => {
      calls.push({ xdr, opts });
      if (decline) return { signedTxXdr: "", signerAddress: "", error: { code: -4, message: "declined" } };
      if (answer) return answer(xdr, opts);
      const tx = StellarSdk.TransactionBuilder.fromXDR(xdr, opts.networkPassphrase);
      tx.sign(key);
      return { signedTxXdr: tx.toXDR(), signerAddress: key.publicKey() };
    },
  };
}
const refusal = async (promise) => {
  try { await promise; } catch (err) { return err; }
  assert.fail("was not refused");
};

test("a signature from the wallet is added to the page's own envelope", async () => {
  const source = StellarSdk.Keypair.random();
  const tx = payment(source);
  const wallet = make(StellarSdk, freighterWith(source), chain);

  const out = await wallet.sign({ xdr: tx.toXDR(), passphrase: PASSPHRASE, address: source.publicKey() });
  const result = StellarSdk.TransactionBuilder.fromXDR(out.xdr, PASSPHRASE);
  assert.ok(result.hash().equals(tx.hash()));
  assert.strictEqual(result.signatures.length, 1);
  assert.ok(source.verify(tx.hash(), result.signatures[0].signature()));
  assert.strictEqual(out.signer, source.publicKey());
});

test("a co-signer adds to an envelope other people have already signed", async () => {
  // The treasury is the source; the signer is one of its signers, not it.
  const treasury = StellarSdk.Keypair.random();
  const ada = StellarSdk.Keypair.random();
  const ben = StellarSdk.Keypair.random();
  const tx = payment(treasury);
  const withAda = signedBy(tx, ada);
  const wallet = make(StellarSdk, freighterWith(ben), chain);

  const out = await wallet.sign({ xdr: tx.toXDR(), onto: withAda, passphrase: PASSPHRASE });
  const result = StellarSdk.TransactionBuilder.fromXDR(out.xdr, PASSPHRASE);
  assert.strictEqual(result.signatures.length, 2, "the signature already there was lost, or the new one was not added");
  assert.ok(ada.verify(tx.hash(), result.signatures[0].signature()));
  assert.ok(ben.verify(tx.hash(), result.signatures[1].signature()));
});

test("a different transaction coming back is never passed on", async () => {
  const source = StellarSdk.Keypair.random();
  const tx = payment(source, "5");
  const other = payment(source, "5000");
  const swapping = freighterWith(source, {
    answer: () => ({ signedTxXdr: signedBy(other, source), signerAddress: source.publicKey() }),
  });
  const err = await refusal(make(StellarSdk, swapping, chain).sign({ xdr: tx.toXDR(), passphrase: PASSPHRASE }));
  assert.match(err.title, /not this transaction/);
  assert.match(err.message, /Nothing was sent/);
});

test("more than the one signature asked for is refused", async () => {
  const source = StellarSdk.Keypair.random();
  const extra = StellarSdk.Keypair.random();
  const tx = payment(source);
  const adding = freighterWith(source, {
    answer: () => ({ signedTxXdr: signedBy(tx, source, extra), signerAddress: source.publicKey() }),
  });
  const err = await refusal(make(StellarSdk, adding, chain).sign({ xdr: tx.toXDR(), passphrase: PASSPHRASE }));
  assert.match(err.message, /exactly one signature/);
});

test("a signature that is not the named account's is refused", async () => {
  const source = StellarSdk.Keypair.random();
  const stranger = StellarSdk.Keypair.random();
  const tx = payment(source);
  // Says it signed as the source; the signature is somebody else's.
  const lying = freighterWith(source, {
    answer: () => ({ signedTxXdr: signedBy(tx, stranger), signerAddress: source.publicKey() }),
  });
  const err = await refusal(make(StellarSdk, lying, chain).sign({ xdr: tx.toXDR(), passphrase: PASSPHRASE }));
  assert.match(err.message, /exactly one signature, from the account it named/);
});

test("the wrong account signing is said, not sent", async () => {
  const wanted = StellarSdk.Keypair.random();
  const active = StellarSdk.Keypair.random();
  const tx = payment(wanted);
  const err = await refusal(make(StellarSdk, freighterWith(active), chain)
    .sign({ xdr: tx.toXDR(), passphrase: PASSPHRASE, address: wanted.publicKey() }));
  assert.match(err.title, /different account/);
  assert.match(err.message, /Switch accounts/);
});

test("signing twice with one account is not two signatures", async () => {
  const treasury = StellarSdk.Keypair.random();
  const ada = StellarSdk.Keypair.random();
  const tx = payment(treasury);
  const err = await refusal(make(StellarSdk, freighterWith(ada), chain)
    .sign({ xdr: tx.toXDR(), onto: signedBy(tx, ada), passphrase: PASSPHRASE }));
  assert.match(err.title, /already signed/);
});

test("a wallet on another network is told which to switch to, before it is shown anything", async () => {
  const source = StellarSdk.Keypair.random();
  const elsewhere = freighterWith(source, { passphrase: PUBLIC });
  const err = await refusal(make(StellarSdk, elsewhere, chain).sign({ xdr: payment(source).toXDR(), passphrase: PASSPHRASE }));
  assert.match(err.title, /different network/);
  assert.match(err.message, /testnet/);
  assert.match(err.message, /public network/);
  assert.strictEqual(elsewhere.calls.length, 0, "the wallet was asked to sign for a network it is not on");
});

test("declining in the wallet leaves the page as it was", async () => {
  const source = StellarSdk.Keypair.random();
  const err = await refusal(make(StellarSdk, freighterWith(source, { decline: true }), chain)
    .sign({ xdr: payment(source).toXDR(), passphrase: PASSPHRASE }));
  assert.match(err.title, /Nothing was signed/);
});

test("with no wallet there is nothing to wait on", async () => {
  const source = StellarSdk.Keypair.random();
  for (const absent of [undefined, {}, freighterWith(source, { connected: false })]) {
    const wallet = make(StellarSdk, absent, chain);
    assert.strictEqual(await wallet.present(), false);
    const err = await refusal(wallet.sign({ xdr: payment(source).toXDR(), passphrase: PASSPHRASE }));
    assert.match(err.title, /not in this browser/);
  }
});

test("something truthy standing where the extension would be is not the extension", async () => {
  // A browser makes a global of every element id, and the library looks for a
  // global called "freighter". A page element with that id is what it finds.
  const element = { tagName: "DIV" };
  const fooled = { isConnected: async () => ({ isConnected: element }) };
  assert.strictEqual(await make(StellarSdk, fooled, chain).present(), false);
});

test("a page offers the button only where the wallet is", async () => {
  const source = StellarSdk.Keypair.random();
  const el = () => ({ hidden: true, disabled: false, listeners: [], attrs: {},
    addEventListener(_e, fn) { this.listeners.push(fn); }, setAttribute(k, v) { this.attrs[k] = v; } });

  const section = el(), button = el(), absent = el();
  const none = make(StellarSdk, undefined, chain);
  assert.strictEqual(await none.offer({ section, button, absent }), false);
  assert.strictEqual(section.hidden, true);
  assert.strictEqual(absent.hidden, false);
  assert.strictEqual(button.listeners.length, 0);

  const tx = payment(source);
  const here = make(StellarSdk, freighterWith(source), chain);
  const s2 = el(), b2 = el();
  let got = null, said = null;
  assert.strictEqual(await here.offer({
    section: s2, button: b2,
    envelope: () => ({ xdr: tx.toXDR(), passphrase: PASSPHRASE }),
    signed: async (r) => { got = r; },
    refused: (t, d) => { said = [t, d]; },
  }), true);
  assert.strictEqual(s2.hidden, false);
  await b2.listeners[0]();
  assert.ok(got && got.xdr, "the page was not handed the signed envelope");
  assert.strictEqual(said, null);
  assert.strictEqual(b2.disabled, false, "the button was left disabled after signing");
});
