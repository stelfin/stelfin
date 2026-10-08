// A button that copies what a text box holds.
//
// Signing in a wallet of one's own means carrying a transaction out of this
// page and a signature back into it. The page asked for that in a sentence —
// "copy the envelope" — and left the doing of it to selecting several hundred
// characters of base64 by hand, on a phone, without picking up the label above
// or dropping the last four characters. One character short is a transaction
// the wallet refuses for a reason it cannot explain.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory;
  } else {
    root.StelfinClipboard = factory(root.navigator);
  }
})(typeof self !== "undefined" ? self : this, function (navigator) {
  "use strict";

  // wire makes button copy source's value, and says in status what happened.
  //
  // It says so either way. A browser inside a chat app often has no clipboard
  // to write to, and a button that then did nothing would look exactly like
  // one that had worked. Where it cannot copy it selects the text, so the one
  // step left is the one the person already knows.
  function wire(button, source, status) {
    if (!button || !source) return;
    button.addEventListener("click", async () => {
      let said = "Copied.";
      try {
        await navigator.clipboard.writeText(source.value);
      } catch (_) {
        if (typeof source.select === "function") source.select();
        said = "This browser will not copy for you. The text is selected: copy it by hand.";
      }
      if (status) {
        status.textContent = said;
        status.hidden = false;
      }
    });
  }

  return { wire };
});
