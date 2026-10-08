// The token a link carried, kept for as long as the tab is.
//
// A link to one of these pages carries its authority in the fragment, so that
// it never reaches a server as part of a URL, and every page takes it straight
// back out of the address bar so that a screenshot or a glance over a shoulder
// does not carry it either. Both of those stay.
//
// What that left was a page that could not be reloaded. The token was in a
// variable and nowhere else, so reloading — which is what a person does when a
// page looks stuck, and what one of these pages told them to do — came back
// with an empty address bar and "This link is incomplete".
//
// So the token is also kept in sessionStorage, under the page's own path. That
// is this tab, on this origin, until the tab closes: it is not shared with
// another tab or window, it is not in history, and it is not sent anywhere. It
// is no more exposed there than in the variable it was already in, to exactly
// the scripts this origin's content policy lets run. A new link opened in the
// same tab replaces it.
(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory;
  } else {
    root.StelfinLink = factory(root.location, root.history, root.sessionStorage);
  }
})(typeof self !== "undefined" ? self : this, function (location, history, storage) {
  "use strict";

  // token returns the link's token, or "" if this page was not opened from a
  // link and has not been in this tab.
  function token() {
    const slot = "stelfin.token:" + location.pathname;
    let value = String(location.hash || "").replace(/^#/, "");

    try {
      if (value) {
        storage.setItem(slot, value);
      } else {
        value = storage.getItem(slot) || "";
      }
    } catch (_) {
      // Storage refused. The page works exactly as it did before this module
      // existed: for as long as it is not reloaded.
    }

    // Out of the address bar either way.
    history.replaceState(null, "", location.pathname);
    return value;
  }

  return { token };
});
