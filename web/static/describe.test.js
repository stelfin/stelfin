// The cross-language half of the guarantee.
//
// The Go renderer writes a corpus: for each transaction, the XDR and the
// canonical description it produced. This reads the same files, re-derives the
// description here, and requires the bytes to match exactly.
//
// Without this, the two implementations agree only by coincidence. The page
// would go on comparing its own output to the server's, both drifting the same
// way, and nobody would learn anything until someone signed the wrong thing.
//
// Run with: make test-js

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const describe = require("./describe.js");

const CORPUS = path.join(__dirname, "..", "..", "settlement", "testdata", "describe");

// Operations the Go renderer describes and this one does not yet.
//
// Listed rather than skipped silently: each name here is a transaction a stelfin
// page currently refuses to let anyone sign, which is the safe direction and
// still a gap. Removing a name means implementing it, not relaxing the test —
// and the test enforces that in both directions, so a name left here after the
// renderer learned the operation fails just as loudly as one removed too soon.
//
// Empty, as of the phase that taught the browser offers, path payments and
// signer changes. Soroban operations will land here when the Go side learns
// them and this side has not yet.
const NOT_YET_IN_THE_BROWSER = new Set([]);

function corpus() {
  return fs
    .readdirSync(CORPUS)
    .filter((f) => f.endsWith(".json"))
    .map((f) => JSON.parse(fs.readFileSync(path.join(CORPUS, f), "utf8")));
}

test("the corpus is not empty", () => {
  const cases = corpus();
  assert.ok(cases.length > 5, `only ${cases.length} cases; the corpus proves little`);
});

test("every case is either reproduced exactly or refused", () => {
  for (const c of corpus()) {
    if (NOT_YET_IN_THE_BROWSER.has(c.name)) {
      assert.throws(
        () => describe.describeTx(c.xdr, c.network),
        describe.Indescribable,
        `${c.name}: listed as unrendered, but it rendered — remove it from the list`
      );
      continue;
    }

    const derived = describe.describeTx(c.xdr, c.network);
    const got = describe.canonical(derived);
    assert.equal(
      got,
      c.canonical,
      `${c.name}: the browser's description differs from the server's\n` +
        `--- browser ---\n${got}\n--- server ---\n${c.canonical}`
    );
  }
});

test("the canonical version matches the server's", () => {
  const one = corpus().find((c) => !NOT_YET_IN_THE_BROWSER.has(c.name));
  assert.ok(one, "no renderable case in the corpus");
  assert.ok(
    one.canonical.startsWith(describe.CANONICAL_VERSION + "\n"),
    "the two sides disagree about the format version, which is what that line is for"
  );
});

test("amounts are normalised without touching a float", () => {
  // The SDK spells amounts one way and Go another; both are the same amount.
  assert.equal(describe.normalizeAmount("5000"), "5000.0000000");
  assert.equal(describe.normalizeAmount("5000.0000000"), "5000.0000000");
  assert.equal(describe.normalizeAmount("0.0000001"), "0.0000001");
  assert.equal(describe.normalizeAmount("-1.5"), "-1.5000000");

  // A value beyond Stellar's precision is not an amount this chain can hold,
  // and rounding it would change what someone agreed to pay.
  assert.throws(() => describe.normalizeAmount("1.00000001"), describe.Indescribable);
  assert.throws(() => describe.normalizeAmount(""), describe.Indescribable);
  assert.throws(() => describe.normalizeAmount("abc"), describe.Indescribable);

  // The number that a float would get wrong.
  assert.equal(describe.normalizeAmount("9007199254740993.0000001"), "9007199254740993.0000001");
});

test("escaping keeps a memo on one line", () => {
  const memoCase = corpus().find((c) => c.name === "payment_memo_with_separators");
  assert.ok(memoCase, "the escaping case is missing from the corpus");

  const canonical = describe.canonical(describe.describeTx(memoCase.xdr, memoCase.network));
  const memoLines = canonical.split("\n").filter((l) => l.startsWith("memo\t"));
  assert.equal(memoLines.length, 1, "a memo containing a newline broke the line format");
  assert.ok(canonical.includes("a\\tb\\nc\\\\d"), "the memo was not escaped as expected");
});

test("an unrenderable operation is refused, not summarised", () => {
  // The rule both sides share: anything not fully rendered stops the signature.
  // A renderer that returned a partial description would let an operation ride
  // along unmentioned, which is the attack the whole layer exists to stop.
  //
  // Inflation is a real operation and deliberately in neither switch, so it
  // stands in for whatever nobody has taught either side yet.
  const inflationTx =
    "AAAAAgAAAACKiOPddAnxlf1S2y08ul1yymcJvx2UEhvzdIgBtA9vXAAAJxAAAAAAAAAAKgAAAAAA" +
    "AAAAAAAAAQAAAAAAAAAJAAAAAAAAAAA=";
  assert.throws(
    () => describe.describeTx(inflationTx, "Test SDF Network ; September 2015"),
    describe.Indescribable
  );
});

test("an offer's price survives as a rational", () => {
  // The parsed operation spells 7/3 as "2.33333333333333333333". Agreeing with
  // the server on a value one side has already rounded is impossible, so the
  // renderer reaches past the parsed form into the envelope.
  const offer = corpus().find((c) => c.name === "manage_sell_offer");
  assert.ok(offer, "the offer case is missing from the corpus");

  const derived = describe.describeTx(offer.xdr, offer.network);
  const price = derived.operations[0].fields.find((f) => f.label === "price");
  assert.equal(price.value, "7/3", "the price was rendered as a decimal");
});

// The total both implementations must produce for the batch_payroll corpus
// case.
//
// Written out here and again in settlement/describe_batch_test.go. Two
// hardcoded strings rather than one shared constant on purpose: the point is
// that two separately-written implementations agree, and sharing the constant
// would let them agree by construction.
const GOLDEN_BATCH_TOTAL = "1500000000.0000003";

test("the browser totals a batch exactly as the server does", () => {
  const policy = require("./policy.js");
  const c = corpus().find((x) => x.name === "batch_payroll");
  assert.ok(c, "the corpus has no batch case");

  const d = describe.describeTx(c.xdr, c.network);
  const got = policy.batch(d, { source: d.source });

  assert.equal(got.total, GOLDEN_BATCH_TOTAL);
  assert.equal(got.rows, 3);
});
