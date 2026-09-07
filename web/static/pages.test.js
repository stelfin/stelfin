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
  "approve.js": ["stellar-sdk.min.js", "describe.js"],
  "reclaim.js": ["stellar-sdk.min.js", "describe.js", "policy.js"],
  "confirm.js": ["stellar-sdk.min.js"],
  "enroll.js": ["stellar-sdk.min.js"],
  "link.js": ["stellar-sdk.min.js"],
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
