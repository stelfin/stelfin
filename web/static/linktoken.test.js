// A page keeps the token its link carried, and keeps it out of the address bar.
const test = require("node:test");
const assert = require("node:assert");
const make = require("./linktoken.js");

// As much of a browser as the module touches.
function browser({ path = "/approve", hash = "", stored = {}, storageThrows = false } = {}) {
  const data = { ...stored };
  const location = { pathname: path, hash };
  const replaced = [];
  const history = { replaceState: (_s, _t, url) => { replaced.push(url); location.hash = ""; } };
  const storage = storageThrows
    ? { getItem() { throw new Error("SecurityError"); }, setItem() { throw new Error("SecurityError"); } }
    : { getItem: (k) => (k in data ? data[k] : null), setItem: (k, v) => { data[k] = String(v); } };
  return { link: make(location, history, storage), location, replaced, data };
}

test("the token comes from the link and leaves the address bar", () => {
  const b = browser({ hash: "#v1.abc.def" });
  assert.strictEqual(b.link.token(), "v1.abc.def");
  assert.deepStrictEqual(b.replaced, ["/approve"], "the token was left where a screenshot would carry it");
});

test("reloading the page does not lose the link", () => {
  // Reloading is what a person does when a page looks stuck. It used to come
  // back as "This link is incomplete".
  const first = browser({ hash: "#v1.abc.def" });
  first.link.token();

  const reloaded = browser({ stored: first.data });
  assert.strictEqual(reloaded.link.token(), "v1.abc.def");
  assert.deepStrictEqual(reloaded.replaced, ["/approve"]);
});

test("a token is kept for the page it was for", () => {
  const approve = browser({ path: "/approve", hash: "#approve-token" });
  approve.link.token();

  // The same tab, another page: an approval's token is not a confirmation's.
  const confirm = browser({ path: "/confirm", stored: approve.data });
  assert.strictEqual(confirm.link.token(), "");
});

test("a new link in the same tab replaces the old one", () => {
  const b = browser({ hash: "#newer", stored: { "stelfin.token:/approve": "older" } });
  assert.strictEqual(b.link.token(), "newer");
  assert.strictEqual(b.data["stelfin.token:/approve"], "newer");
});

test("a page opened from nowhere has no token", () => {
  assert.strictEqual(browser().link.token(), "");
});

test("storage that refuses changes nothing but reloading", () => {
  const b = browser({ hash: "#v1.abc.def", storageThrows: true });
  assert.strictEqual(b.link.token(), "v1.abc.def");
  assert.deepStrictEqual(b.replaced, ["/approve"]);
  assert.strictEqual(browser({ storageThrows: true }).link.token(), "");
});
