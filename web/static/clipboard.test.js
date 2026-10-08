// The copy button copies, and says what happened whether it could or not.
const test = require("node:test");
const assert = require("node:assert");
const make = require("./clipboard.js");

function button() {
  const b = { listeners: [], addEventListener(_e, fn) { b.listeners.push(fn); } };
  b.click = () => Promise.all(b.listeners.map((fn) => fn()));
  return b;
}

test("it copies exactly what the box holds", async () => {
  let written = null;
  const clip = make({ clipboard: { writeText: async (t) => { written = t; } } });
  const b = button();
  const status = { hidden: true, textContent: "" };
  clip.wire(b, { value: "AAAAAgAAAAB+envelope==" }, status);

  await b.click();
  assert.strictEqual(written, "AAAAAgAAAAB+envelope==");
  assert.strictEqual(status.hidden, false);
  assert.strictEqual(status.textContent, "Copied.");
});

test("where it cannot copy it selects the text and says so", async () => {
  // A browser inside a chat app often has no clipboard to write to. A button
  // that then did nothing would look exactly like one that had worked.
  for (const navigator of [{}, { clipboard: { writeText: async () => { throw new Error("NotAllowedError"); } } }]) {
    const clip = make(navigator);
    const b = button();
    let selected = false;
    const status = { hidden: true, textContent: "" };
    clip.wire(b, { value: "x", select() { selected = true; } }, status);

    await b.click();
    assert.ok(selected, "the text was not selected for copying by hand");
    assert.strictEqual(status.hidden, false);
    assert.match(status.textContent, /by hand/);
    assert.doesNotMatch(status.textContent, /^Copied/);
  }
});

test("a page without the button is left alone", () => {
  const clip = make({});
  clip.wire(null, { value: "x" }, null);
  clip.wire(button(), null, null);
});
