// Package web serves the confirmation page.
//
// The assets are embedded in the binary rather than read from disk, so a
// deployment cannot end up serving a page that does not match the server it is
// talking to — the confirmation screen and the API that authorises payments
// ship as one artifact.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var assets embed.FS

// Static returns the embedded asset filesystem, rooted at the asset directory.
func Static() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		// Only reachable if the embed directive and this path disagree, which
		// is a build-time mistake rather than a runtime condition.
		panic("web: embedded assets are missing: " + err.Error())
	}
	return sub
}

// Handler serves the confirmation page and its assets.
//
// The response headers are the page's security posture, and they are set here
// rather than in a proxy so they cannot be lost by a deployment change:
//
//   - A strict CSP with no 'unsafe-inline' for scripts. The page's own script
//     lives in its own file for exactly this reason — an inline script would
//     force the policy open and remove most of its value.
//   - connect-src 'self': the page may only talk back to the origin that
//     served it, so a script injected despite the policy still cannot
//     exfiltrate a signing key to another host.
//   - no-referrer: the confirmation token rides in the URL fragment, and this
//     keeps it out of any outbound request.
//   - no-store: a payment confirmation should not sit in a shared cache or a
//     browser's back-forward cache.
func Handler() http.Handler {
	files := http.FileServer(http.FS(Static()))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'none'",
			"script-src 'self'",
			// Styles are inline in the document; unlike script, that carries
			// no ability to read or send anything.
			"style-src 'unsafe-inline'",
			"connect-src 'self'",
			"img-src 'self' data:",
			"base-uri 'none'",
			"form-action 'none'",
			"frame-ancestors 'none'",
		}, "; "))
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")

		// /confirm and /enroll are pages; /static/ is a prefix over the same
		// root Static() already sits at, so it must be stripped rather than
		// passed straight through — otherwise a request for /static/confirm.js
		// resolves against static/static/confirm.js inside the embedded FS,
		// which does not exist. Everything else is served as-is.
		switch {
		case r.URL.Path == "/confirm" || r.URL.Path == "/confirm/":
			r = r.Clone(r.Context())
			r.URL.Path = "/confirm.html"
		case r.URL.Path == "/enroll" || r.URL.Path == "/enroll/":
			r = r.Clone(r.Context())
			r.URL.Path = "/enroll.html"
		case strings.HasPrefix(r.URL.Path, "/static/"):
			r = r.Clone(r.Context())
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/static")
		}
		files.ServeHTTP(w, r)
	})
}
