// A network is named for people, and a transaction is linked only when it can be.
const test = require("node:test");
const assert = require("node:assert");
const chain = require("./chain.js");

const TESTNET = "Test SDF Network ; September 2015";
const PUBLIC = "Public Global Stellar Network ; September 2015";
const HASH = "02b96d5958a5b23127e9faa1275dd8d52a32ef30b82284b1430ce57ea1da3430";

test("a network is called what a person calls it", () => {
  assert.match(chain.name(TESTNET), /testnet/);
  // The one question somebody has: is this real money.
  assert.match(chain.name(TESTNET), /no real money/);
  assert.doesNotMatch(chain.name(PUBLIC), /no real money/);
  assert.match(chain.name(PUBLIC), /public/);
});

test("an unfamiliar network is shown as it is, not papered over", () => {
  assert.strictEqual(chain.name("Some Fork ; 2030"), "Some Fork ; 2030");
  assert.strictEqual(chain.name(undefined), "");
});

test("a transaction is linked on the network it is on", () => {
  assert.strictEqual(chain.txURL(TESTNET, HASH), "https://stellar.expert/explorer/testnet/tx/" + HASH);
  assert.strictEqual(chain.txURL(PUBLIC, HASH), "https://stellar.expert/explorer/public/tx/" + HASH);
});

test("nothing is linked that is not a transaction hash on a known network", () => {
  for (const hash of ["", "provisioned", HASH.toUpperCase(), HASH + "0", HASH.slice(1), "../../account/x", undefined]) {
    assert.strictEqual(chain.txURL(TESTNET, hash), "", `linked ${JSON.stringify(hash)}`);
  }
  assert.strictEqual(chain.txURL("Some Fork ; 2030", HASH), "");
});

test("pointing a link shows it, opens elsewhere, and carries nothing along", () => {
  const anchor = { hidden: true };
  assert.strictEqual(chain.point(anchor, TESTNET, HASH), true);
  assert.strictEqual(anchor.hidden, false);
  assert.strictEqual(anchor.target, "_blank");
  assert.strictEqual(anchor.rel, "noopener noreferrer");
  assert.ok(anchor.href.endsWith("/tx/" + HASH));

  // With nowhere to point it stays hidden rather than becoming a dead link.
  const dead = { hidden: true };
  assert.strictEqual(chain.point(dead, TESTNET, "not-a-hash"), false);
  assert.strictEqual(dead.hidden, true);
  assert.strictEqual(dead.href, undefined);
  assert.strictEqual(chain.point(null, TESTNET, HASH), false);
});
