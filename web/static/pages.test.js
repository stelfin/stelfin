// The pages load the modules they use, in an order that works.
//
// Everything else in this directory is tested under node, where `require`
// resolves dependencies itself. The browser has no such thing: each module is a
// script tag that defines a global, and a page that forgets one — or loads it
// after the module that reads it — fails at runtime with a TypeError rather
// than refusing to sign.
//
// That failure mode is the reason this file exists. A page that cannot render a
// transaction must refuse; a page that throws while trying to has no defined
// behaviour at all, and the JS suite would never notice because it never loads
// a page.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

// What each module needs already defined when it runs.
const NEEDS = {
  // The vendored SDK, which every other module reads through a global.
  "stellar-sdk.min.js": [],
  "describe.js": ["stellar-sdk.min.js", "scval.js"],
  "scval.js": ["stellar-sdk.min.js"],
  "policy.js": [],
  // Reads the link's token and keeps it for the tab, so a page can be reloaded.
  "linktoken.js": [],
  // Names a network for people, and links a transaction on it.
  "chain.js": [],
  // The button that copies what a text box holds.
  "clipboard.js": [],
  // Freighter's own library: how a page speaks to the browser extension.
  "freighter-api.min.js": [],
  // Signs with a wallet in the browser, and checks what it gets back.
  "wallet.js": ["stellar-sdk.min.js", "freighter-api.min.js", "chain.js"],
  // Finds the key this device holds for a wallet, by the wallet's address.
  "devicekey.js": ["stellar-sdk.min.js"],
  "approve.js": ["linktoken.js", "stellar-sdk.min.js", "describe.js", "policy.js", "chain.js", "clipboard.js", "wallet.js"],
  "reclaim.js": ["linktoken.js", "stellar-sdk.min.js", "describe.js", "policy.js", "devicekey.js", "chain.js", "clipboard.js", "wallet.js"],
  "confirm.js": ["linktoken.js", "stellar-sdk.min.js", "devicekey.js", "chain.js", "clipboard.js", "wallet.js"],
  "enroll.js": ["linktoken.js", "stellar-sdk.min.js"],
  "link.js": ["linktoken.js", "stellar-sdk.min.js", "devicekey.js", "chain.js", "clipboard.js", "wallet.js"],
};

function scriptsIn(html) {
  const out = [];
  const re = /<script\s+src="\/static\/([^"]+)"><\/script>/g;
  let m;
  while ((m = re.exec(html)) !== null) out.push(m[1]);
  return out;
}

const pages = fs.readdirSync(__dirname).filter((f) => f.endsWith(".html"));

test("every page loads its modules, in a working order", () => {
  assert.ok(pages.length > 0, "no pages found");

  for (const page of pages) {
    const scripts = scriptsIn(fs.readFileSync(path.join(__dirname, page), "utf8"));
    scripts.forEach((script, i) => {
      const needs = NEEDS[script];
      assert.ok(needs, `${page} loads ${script}, which this test does not know about`);
      for (const dep of needs) {
        const at = scripts.indexOf(dep);
        assert.ok(
          at !== -1,
          `${page} loads ${script} without ${dep}, so it will throw rather than refuse`
        );
        assert.ok(
          at < i,
          `${page} loads ${dep} after ${script}, so the global is not defined yet`
        );
      }
    });
  }
});

test("every module a page can load is one this test knows about", () => {
  // Keeps the table above honest: a new module added to a page without a line
  // here fails loudly rather than being silently unchecked.
  for (const name of Object.keys(NEEDS)) {
    assert.ok(
      fs.existsSync(path.join(__dirname, name)),
      `${name} is listed but does not exist`
    );
  }
});

test("no page constructs what the SDK only offers as a function", () => {
  // `new StellarSdk.TransactionBuilder.fromXDR(…)` reads like a constructor
  // call and is not one: fromXDR is a static method, and `new` on it throws
  // "is not a constructor" before a single byte of the envelope is read. Four
  // of the five pages once did exactly that, and nothing here noticed, because
  // nothing here loads a page — every wallet link and every confirmation was
  // dead in a real browser while this suite stayed green.
  const sdk = require("./stellar-sdk.min.js");
  assert.throws(
    () => new sdk.TransactionBuilder.fromXDR("AAAA", "x"),
    /not a constructor/,
    "the premise of this test: the SDK refuses `new` on fromXDR"
  );

  const scripts = fs
    .readdirSync(__dirname)
    .filter((f) => f.endsWith(".js") && !f.endsWith(".test.js") && !f.endsWith(".min.js"));
  assert.ok(scripts.length > 0, "no page scripts found");
  for (const script of scripts) {
    const source = fs.readFileSync(path.join(__dirname, script), "utf8");
    assert.doesNotMatch(
      source,
      /new\s+StellarSdk\.[\w.]*\.from\w+\(/,
      `${script} calls a static SDK method with \`new\`, which throws in the browser`
    );
  }
});

test("every vendored script is the one it is recorded as", () => {
  // A vendored file nobody can identify is its own problem: there is no way to
  // tell an upgrade from a substitution. These run on the pages where people
  // sign, under a content policy that trusts this origin and nothing else.
  const crypto = require("node:crypto");
  const vendor = JSON.parse(fs.readFileSync(path.join(__dirname, "VENDOR.json"), "utf8")).files;

  for (const [name, entry] of Object.entries(vendor)) {
    const file = path.join(__dirname, name);
    assert.ok(fs.existsSync(file), `${name} is recorded as vendored and is not here`);
    const hash = crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
    assert.strictEqual(
      hash,
      entry.sha256,
      `${name} is not ${entry.package}@${entry.version}: it has been changed, or upgraded without its record`
    );
    for (const field of ["package", "version", "source", "license"]) {
      assert.ok(entry[field], `${name} has no ${field} recorded`);
    }
  }

  // And nothing minified is here that the record does not know about.
  for (const name of fs.readdirSync(__dirname).filter((f) => f.endsWith(".min.js"))) {
    assert.ok(vendor[name], `${name} is in this directory and is not recorded in VENDOR.json`);
  }
});

test("no page names an element after a global a wallet library looks for", () => {
  // A browser exposes every element id as a property of window. Freighter's
  // library decides the extension is installed by checking window.freighter,
  // so a page with id="freighter" tells it the extension is there — and the
  // page then offers a button that waits for ever on a wallet that is not.
  // That shipped to the browser test once, as the id of the very section the
  // button lives in.
  for (const page of pages) {
    const html = fs.readFileSync(path.join(__dirname, page), "utf8");
    for (const taken of ["freighter", "freighterApi", "StellarSdk"]) {
      assert.ok(
        !new RegExp(`\\bid="${taken}"`).test(html),
        `${page} has an element with id="${taken}", which shadows a global its scripts read`
      );
    }
  }
});
