// Walks the signing pages in a real browser, against the state `make e2e`
// holds open.
//
// The Go half of the end-to-end test plays the browser by calling the JSON
// endpoints directly, which proves the server and proves nothing about the
// pages. Four of the five once threw before rendering a thing while every
// other suite was green, because nothing loaded them. This does: it opens each
// link the bot handed out, reads what the page shows, and presses the buttons
// a person would press.
//
// No dependencies. It speaks the DevTools protocol to a Chrome started with
// --remote-debugging-port, over the WebSocket that ships with Node 22.
//
//   node walk.mjs <hold-file> <devtools-port>
import { readFileSync, existsSync } from "node:fs";

const [holdFile, port = "9333"] = process.argv.slice(2);
const PASSPHRASE = "Test SDF Network ; September 2015";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// The Go test appends a line when the state behind it is ready, so a line that
// is not there yet is waited for rather than treated as missing.
async function held(label, minutes = 10) {
  for (let i = 0; i < minutes * 30; i++) {
    if (existsSync(holdFile)) {
      const line = readFileSync(holdFile, "utf8").split("\n").find((l) => l.startsWith(label));
      if (line) return line.split(/\s+/).pop();
    }
    await sleep(2000);
  }
  throw new Error(`the test never offered "${label}"`);
}

// The browser was started a moment ago by whoever started this, and may not be
// listening yet.
for (let i = 0; ; i++) {
  try {
    await fetch(`http://127.0.0.1:${port}/json/version`);
    break;
  } catch (err) {
    if (i === 60) throw new Error(`no browser is listening on port ${port}: ${err.message}`);
    await sleep(500);
  }
}

const pageErrors = [];

async function open(name, url) {
  const target = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" })).json();
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject; });

  let id = 0;
  const waiting = new Map();
  ws.onmessage = (m) => {
    const d = JSON.parse(m.data);
    if (d.id && waiting.has(d.id)) { waiting.get(d.id)(d); waiting.delete(d.id); }
    if (d.method === "Runtime.exceptionThrown") {
      const e = d.params.exceptionDetails;
      pageErrors.push(`${name}: ${e.exception?.description || e.text}`);
    }
  };
  const send = (method, params = {}) =>
    new Promise((resolve) => { waiting.set(++id, resolve); ws.send(JSON.stringify({ id, method, params })); });

  await send("Runtime.enable");
  await send("Page.enable");
  await send("Page.navigate", { url });

  const run = async (body) => {
    const r = await send("Runtime.evaluate", {
      expression: `(async () => { ${body} })()`, awaitPromise: true, returnByValue: true,
    });
    if (r.result.exceptionDetails) {
      throw new Error(`${name}: ${r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text}`);
    }
    return r.result.result.value;
  };
  const text = (id) => run(`return document.getElementById(${JSON.stringify(id)})?.innerText ?? ""`);
  const until = async (condition, what) => {
    for (let i = 0; i < 240; i++) {
      if (await run(`return Boolean(${condition})`)) return;
      await sleep(500);
    }
    throw new Error(`${name}: timed out waiting for ${what}. The page shows:\n` +
      (await run("return document.body.innerText")));
  };
  return { run, text, until };
}

const shown = (id) => `(document.getElementById("${id}") && !document.getElementById("${id}").hidden)`;
// A refusal, as opposed to a progress notice, is the alert box in its assertive role.
const refused = `(${shown("alert")} && document.getElementById("alert").getAttribute("role") !== "status")`;
const outcome = async (page, doneId) =>
  (await page.text((await page.run(`return ${shown(doneId)}`)) ? doneId : "alert")).replace(/\s+/g, " ").trim();

function check(what, ok, detail = "") {
  console.log(`${ok ? "ok  " : "FAIL"}  ${what}${detail ? " — " + detail : ""}`);
  if (!ok) process.exitCode = 1;
}

// Signs whatever envelope a page is showing, the way a wallet the page has
// never heard of would: outside it, with a key it does not hold.
const signedWith = (box, ...seeds) => `
  const tx = StellarSdk.TransactionBuilder.fromXDR(document.getElementById("${box}").value, "${PASSPHRASE}");
  for (const seed of ${JSON.stringify(seeds)}) tx.sign(StellarSdk.Keypair.fromSecret(seed));
  return tx.toXDR();`;
const paste = (box, value, button) =>
  `document.getElementById("${box}").value = ${JSON.stringify(value)}; document.getElementById("${button}").click();`;

let signed;

// ---- Enrol. First, because the link is good for two minutes.

const enroll = await open("enroll", await held("enroll (dave)"));
await enroll.until(`${shown("pending")} || ${refused}`, "the new wallet");
const wallet = await enroll.text("address");
check("enroll page generated a wallet on the device", /^G[A-Z2-7]{55}$/.test(wallet), wallet);
check("the token is gone from the address bar", !(await enroll.run("return location.href")).includes("#"));

// Reloading is what a person does when a page looks stuck. It used to come back
// as "This link is incomplete", and — because the key was stored as if the
// wallet already existed — as "this device already has a wallet" on every link
// after it. The page has to come back as the same page, for the same wallet.
check("a key made for a wallet that does not exist yet is filed as unfinished",
  await enroll.run(`return localStorage.getItem("stelfin.key") === null && localStorage.getItem("stelfin.key.pending") !== null`));
await enroll.run(`setTimeout(() => location.reload(), 0)`);
await sleep(1500);
await enroll.until(`${shown("pending")} || ${refused}`, "the page to come back after a reload");
check("a reloaded page still has its link", !(await enroll.run(`return ${refused}`)),
  (await enroll.run(`return ${refused}`)) ? (await enroll.text("alert")).replace(/\s+/g, " ") : "");
check("a reloaded page carries on with the same wallet", (await enroll.text("address")) === wallet, await enroll.text("address"));
check("the token is still out of the address bar after a reload", !(await enroll.run("return location.href")).includes("#"));

await enroll.until(`!document.getElementById("create").disabled || ${refused}`, "the page to verify its transaction");
check("enroll page verified the transaction it was sent", !(await enroll.run(`return ${refused}`)),
  (await enroll.run(`return ${refused}`)) ? (await enroll.text("alert")).replace(/\s+/g, " ") : "");
await enroll.run(`document.getElementById("create").click()`);
await enroll.until(`${shown("done")} || ${refused}`, "the account to be created");
const enrolled = await enroll.run(`return ${shown("done")}`);
check("enroll page created the account", enrolled, await outcome(enroll, "done"));
check("once the wallet exists its key is filed where the other pages look",
  await enroll.run(`return localStorage.getItem("stelfin.key.pending") === null &&
    StellarSdk.Keypair.fromSecret(localStorage.getItem("stelfin.key")).publicKey() === ${JSON.stringify(wallet)}`));
// The one moment a copy of the key can be taken. Not shown until asked for.
check("the secret key is not on screen until asked for", !(await enroll.run(`return ${shown("backup")}`)));
await enroll.run(`document.getElementById("reveal").click()`);
check("the secret key can be shown for a backup, and is this wallet's",
  await enroll.run(`const s = document.getElementById("secret").textContent;
    return ${shown("backup")} && /^S[A-Z2-7]{55}$/.test(s) && StellarSdk.Keypair.fromSecret(s).publicKey() === ${JSON.stringify(wallet)}`));

// ---- Confirm, for a wallet whose key is not on this device.
//
// Eve linked her own wallet, so the page has nothing to sign with and that is
// not an error: it hands the transaction over and takes it back signed.

const own = await open("confirm (own wallet)", await held("confirm (eve)"));
await own.until(`${shown("payment")} || ${refused}`, "the payment");
check("confirm page offers the transaction instead of a dead end",
  await own.run(`return ${shown("own")} && !${shown("device")} && !${shown("alert")}`));
// Pasting the envelope back unsigned is the obvious mistake.
await own.run(paste("signed", await own.run(`return document.getElementById("envelope").value`), "submitSigned"));
await own.until(shown("alert"), "the unsigned notice");
check("an unsigned paste is refused and can be corrected",
  await own.run(`return ${shown("payment")}`), (await own.text("alert")).replace(/\s+/g, " "));
signed = await own.run(signedWith("envelope", await held("eve seed")));
await own.run(paste("signed", signed, "submitSigned"));
await own.until(`document.getElementById("paymentNote").textContent.startsWith("Sent") || (${refused} && !${shown("payment")})`, "the payment to send");
check("confirm page sent a payment signed in another wallet",
  (await own.text("paymentNote")).startsWith("Sent"), await own.text("paymentNote"));

// ---- Approve, as two signers, then execute from the page.

const ada = await open("approve (ada)", await held("approve (ada)"));
await ada.until(`${shown("proposal")} || ${refused}`, "the proposal");
check("approve page derived the payment from the envelope",
  (await ada.text("operations")).includes("payment"), (await ada.text("operations")).replace(/\s+/g, " ").slice(0, 90));
signed = await ada.run(signedWith("envelope", await held("ada seed")));
await ada.run(paste("signed_xdr", signed, "submit"));
// Until the button is free again, and not merely until the figure reads one:
// it may have read one before this signature went in, and a click on a button
// that is still busy is a click that does nothing.
await ada.until(`document.getElementById("weight").textContent.startsWith("1 of") && !document.getElementById("submit").disabled`,
  "the first signature to count");
check("one signature of two does not offer execution", !(await ada.run(`return ${shown("ready")}`)), await ada.text("weight"));

const stranger = await ada.run(`
  const tx = StellarSdk.TransactionBuilder.fromXDR(document.getElementById("envelope").value, "${PASSPHRASE}");
  tx.sign(StellarSdk.Keypair.random()); return tx.toXDR();`);
await ada.run(paste("signed_xdr", stranger, "submit"));
await ada.until(shown("alert"), "the refusal");
check("a stranger's signature is refused and the page stays usable",
  await ada.run(`return ${shown("proposal")}`), (await ada.text("alert")).replace(/\s+/g, " "));

const ben = await open("approve (ben)", await held("approve (ben)"));
await ben.until(`${shown("proposal")} || ${refused}`, "the proposal");
signed = await ben.run(signedWith("envelope", await held("ben seed")));
await ben.run(paste("signed_xdr", signed, "submit"));
await ben.until(`${shown("ready")} || ${refused}`, "the execute button");
await ben.run(`document.getElementById("execute").click()`);
await ben.until(`${shown("done")} || ${refused}`, "execution");
check("approve page executed the proposal", await ben.run(`return ${shown("done")}`), await outcome(ben, "done"));
check("the executed transaction can be looked up",
  await ben.run(`const a = document.getElementById("explorer");
    const at = "https://stellar.expert/explorer/testnet/tx/";
    return !a.hidden && a.href.startsWith(at) && a.href.length === at.length + 64 && a.rel === "noopener noreferrer"`),
  await ben.run(`return document.getElementById("explorer").href`));
check("the envelope can be copied with a button, and the fee is in XLM",
  await ada.run(`return Boolean(document.getElementById("copyEnvelope")) && /XLM$/.test(document.getElementById("fee").textContent)`),
  await ada.text("fee"));

// ---- Link a wallet, and prove a treasury with two signatures.

const link = await open("link", await held("link (ben)"));
await link.until(`${shown("pending")} || ${refused}`, "the challenge");
check("link page shows an unsubmittable challenge", (await link.text("sequence")) === "0");
signed = await link.run(signedWith("challenge", await held("ben new wallet")));
await link.run(paste("signed", signed, "submit"));
await link.until(`${shown("done")} || ${refused}`, "the link");
check("link page linked the wallet", await link.run(`return ${shown("done")}`), await outcome(link, "done"));

const treasury = await open("treasury", await held("treasury (ada)"));
await treasury.until(`${shown("pending")} || ${refused}`, "the treasury challenge");
const one = await treasury.run(signedWith("challenge", await held("ada seed")));
await treasury.run(`document.getElementById("label").value = "ops";` + paste("signed", one, "submit"));
await treasury.until(shown("alert"), "the partial-signature notice");
check("one signature on a 2-of-3 treasury is progress, not proof",
  await treasury.run(`return ${shown("pending")} && !${shown("done")}`), (await treasury.text("alert")).replace(/\s+/g, " "));
const two = await treasury.run(`
  const tx = StellarSdk.TransactionBuilder.fromXDR(${JSON.stringify(one)}, "${PASSPHRASE}");
  tx.sign(StellarSdk.Keypair.fromSecret(${JSON.stringify(await held("ben seed"))})); return tx.toXDR();`);
await treasury.run(paste("signed", two, "submit"));
await treasury.until(`${shown("done")} || ${refused}`, "the treasury proof");
check("link page proved the treasury", await treasury.run(`return ${shown("done")}`), await outcome(treasury, "done"));

// ---- Grant a connector: a statement, hashed here, signed at spending weight.

const grant = await open("grant", await held("grant (ada)"));
await grant.until(`${shown("pending")} || ${refused}`, "the grant");
const stated = (await grant.text("statement")).replace(/\s+/g, " ");
check("grant page shows the statement, having hashed it to the digest in the envelope",
  (await grant.run(`return ${shown("grant")}`)) && stated.includes("payroll") && stated.includes("read.range"),
  stated.slice(0, 110));
check("grant page says what the connector cannot do", stated.includes("sign, submit or move funds"));
const half = await grant.run(signedWith("challenge", await held("ada seed")));
await grant.run(paste("signed", half, "submit"));
await grant.until(shown("alert"), "the partial-signature notice");
check("one signature of two does not grant a connector",
  await grant.run(`return ${shown("pending")} && !${shown("done")}`), (await grant.text("alert")).replace(/\s+/g, " "));
await grant.until(`!document.getElementById("submit").disabled`, "the page to be ready for the next signature");
const whole = await grant.run(`
  const tx = StellarSdk.TransactionBuilder.fromXDR(${JSON.stringify(half)}, "${PASSPHRASE}");
  tx.sign(StellarSdk.Keypair.fromSecret(${JSON.stringify(await held("ben seed"))})); return tx.toXDR();`);
await grant.run(paste("signed", whole, "submit"));
await grant.until(`${shown("done")} || ${refused}`, "the grant to be put in force");
check("grant page put the grant in force", await grant.run(`return ${shown("done")}`), await outcome(grant, "done"));

// ---- Set a connector up: an address typed on a page, and refused if private.

const setup = await open("setup", await held("setup (ada)"));
await setup.until(`${shown("setup")} || ${refused}`, "the set-up form");
check("set-up page names the connector and offers no challenge",
  (await setup.text("setupName")) === "rates" && !(await setup.run(`return ${shown("pending")}`)));
check("the token field does not show what is typed",
  (await setup.run(`return document.getElementById("credential").type`)) === "password");
const tryAddress = async (address) => {
  await setup.run(`document.getElementById("alert").hidden = true;
    document.getElementById("endpoint").value = ${JSON.stringify(address)};
    document.getElementById("save").click();`);
  await setup.until(`${shown("alert")} && !document.getElementById("save").disabled`, "an answer about " + address);
  return (await setup.text("alert")).replace(/\s+/g, " ");
};
let answer = await tryAddress("https://169.254.169.254/latest/meta-data/");
check("an address inside the network is refused, and the form stays",
  answer.includes("cannot be used") && (await setup.run(`return ${shown("setup")}`)), answer);
answer = await tryAddress("https://horizon-testnet.stellar.org/");
check("a public address that serves no rows is tried, and reported as not useful",
  answer.includes("did not answer usefully") && (await setup.run(`return ${shown("setup")}`)), answer);

// Dave's last two pages depend on the wallet his browser made on the first.
if (!enrolled) {
  check("confirm and reclaim pages", false, "not reached: there is no wallet to pay from or hand back");
} else {
  // ---- Confirm a payment typed as a sentence, signed with the device's key.

  const confirm = await open("confirm", await held("confirm (dave)"));
  await confirm.until(`${shown("payment")} || ${refused}`, "the payment");
  check("confirm page shows what the envelope pays",
    (await confirm.text("amount")).startsWith("3") && (await confirm.text("asset")) === "USDC",
    `${await confirm.text("amount")} ${await confirm.text("asset")} to ${await confirm.text("toAddress")}`);
  check("confirm page found the key the enroll page left on this device",
    !(await confirm.run(`return document.getElementById("confirm").disabled`)));
  await confirm.run(`document.getElementById("confirm").click()`);
  await confirm.until(`document.getElementById("paymentNote").textContent.startsWith("Sent") || ${refused}`, "the payment to send");
  check("confirm page sent the payment", (await confirm.text("paymentNote")).startsWith("Sent"),
    (await confirm.run(`return ${refused}`)) ? await confirm.text("alert") : await confirm.text("paymentNote"));

  // ---- Hand the wallet back.

  const reclaim = await open("reclaim", await held("reclaim (dave)"));
  await reclaim.until(`${shown("reclaim")} || ${refused}`, "the hand-back");
  check("reclaim page names the wallet this device made", (await reclaim.text("address")) === wallet);
  check("reclaim page will not sign until the address is typed back",
    await reclaim.run(`return document.getElementById("sign").disabled`));
  await reclaim.run(`
    const box = document.getElementById("confirm");
    box.value = document.getElementById("tail").textContent;
    box.dispatchEvent(new Event("input", { bubbles: true }));`);
  await reclaim.run(`document.getElementById("sign").click()`);
  await reclaim.until(`${shown("done")} || ${refused}`, "the account to be handed back");
  check("reclaim page handed the account back", await reclaim.run(`return ${shown("done")}`), await outcome(reclaim, "done"));
  // The account is gone, so its key opens nothing. Left behind it is what made
  // the page that sets a wallet up say there already was one, to somebody who
  // had just handed theirs back.
  check("a handed-back wallet's key is not left on the device",
    await reclaim.run(`return localStorage.getItem("stelfin.key") === null && localStorage.getItem("stelfin.key.pending") === null`));
  check("the hand-back can be looked up",
    await reclaim.run(`const a = document.getElementById("explorer"); return !a.hidden && a.href.includes("/explorer/testnet/tx/")`));
}

check("no page threw", pageErrors.length === 0, pageErrors.join(" | "));
process.exit(process.exitCode ?? 0);
