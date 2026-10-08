// The device key is found by the address it is for, and by nothing else.
const test = require("node:test");
const assert = require("node:assert");
const StellarSdk = require("./stellar-sdk.min.js");
const make = require("./devicekey.js");

// A localStorage, as much of one as the module uses.
function storage(initial = {}) {
  const data = { ...initial };
  return {
    data,
    getItem: (k) => (k in data ? data[k] : null),
    setItem: (k, v) => { data[k] = String(v); },
    removeItem: (k) => { delete data[k]; },
  };
}

const wallet = () => StellarSdk.Keypair.random();

test("a key is returned only for the wallet it belongs to", () => {
  const mine = wallet();
  const other = wallet();
  const keys = make(StellarSdk, storage({ "stelfin.key": mine.secret() }));

  assert.strictEqual(keys.keyFor(mine.publicKey()).secret(), mine.secret());
  // Having a key is not having the key. One for another wallet would sign
  // without complaint, and the network would refuse what the page called sent.
  assert.strictEqual(keys.keyFor(other.publicKey()), null);
  assert.strictEqual(keys.keyFor(""), null);
  assert.strictEqual(keys.keyFor(undefined), null);
});

test("a key left under unfinished is found, and filed where it belongs", () => {
  // The request that created the wallet succeeded and its answer was lost, so
  // the page that made the key never learned the wallet exists.
  const mine = wallet();
  const store = storage({ "stelfin.key.pending": mine.secret() });
  const keys = make(StellarSdk, store);

  const found = keys.keyFor(mine.publicKey());
  assert.ok(found, "the wallet exists and its key is on this device, and the page could not sign");
  assert.strictEqual(found.secret(), mine.secret());
  assert.strictEqual(store.data["stelfin.key"], mine.secret());
  assert.ok(!("stelfin.key.pending" in store.data));
});

test("an unfinished key is not mistaken for some other wallet's", () => {
  const unfinished = wallet();
  const asked = wallet();
  const store = storage({ "stelfin.key.pending": unfinished.secret() });
  const keys = make(StellarSdk, store);

  assert.strictEqual(keys.keyFor(asked.publicKey()), null);
  // And it is left exactly where it was, for the attempt it belongs to.
  assert.strictEqual(store.data["stelfin.key.pending"], unfinished.secret());
  assert.ok(!("stelfin.key" in store.data));
});

test("filing an unfinished key never overwrites a wallet's key", () => {
  const existing = wallet();
  const unfinished = wallet();
  const store = storage({
    "stelfin.key": existing.secret(),
    "stelfin.key.pending": unfinished.secret(),
  });
  const keys = make(StellarSdk, store);

  assert.strictEqual(keys.keyFor(unfinished.publicKey()).secret(), unfinished.secret());
  assert.strictEqual(store.data["stelfin.key"], existing.secret(), "a wallet's key was overwritten");
});

test("forgetting removes the key for that wallet and no other", () => {
  const gone = wallet();
  const kept = wallet();
  const store = storage({ "stelfin.key": kept.secret(), "stelfin.key.pending": gone.secret() });
  const keys = make(StellarSdk, store);

  keys.forget(gone.publicKey());
  assert.ok(!("stelfin.key.pending" in store.data));
  assert.strictEqual(store.data["stelfin.key"], kept.secret());

  keys.forget(kept.publicKey());
  assert.deepStrictEqual(store.data, {});

  // Nothing to forget is not an error.
  keys.forget(wallet().publicKey());
  keys.forget("");
});

test("storage that refuses, or holds rubbish, means no key", () => {
  const refusing = {
    getItem() { throw new Error("SecurityError"); },
    setItem() { throw new Error("SecurityError"); },
    removeItem() { throw new Error("SecurityError"); },
  };
  assert.strictEqual(make(StellarSdk, refusing).keyFor(wallet().publicKey()), null);
  make(StellarSdk, refusing).forget(wallet().publicKey());

  const rubbish = make(StellarSdk, storage({ "stelfin.key": "not a seed", "stelfin.key.pending": "nor this" }));
  assert.strictEqual(rubbish.keyFor(wallet().publicKey()), null);
});
