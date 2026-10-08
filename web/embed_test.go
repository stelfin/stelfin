package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestEveryPageIsServedWithItsHeaders: the headers are the pages' security
// posture, set here so that a deployment change cannot lose them.
func TestEveryPageIsServedWithItsHeaders(t *testing.T) {
	for _, path := range []string{"/confirm", "/enroll", "/link", "/approve", "/reclaim", "/call"} {
		rec := get(t, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)
			continue
		}
		h := rec.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: the content policy lacks %q: %s", path, want, csp)
			}
		}
		// 'unsafe-inline' is allowed for style and for nothing else. On the
		// script directive it would let an injected tag run, which is most of
		// what the policy is for.
		for _, directive := range strings.Split(csp, ";") {
			directive = strings.TrimSpace(directive)
			if strings.HasPrefix(directive, "script-src") && directive != "script-src 'self'" {
				t.Errorf("%s: scripts may come from more than this origin: %q", path, directive)
			}
		}
		for name, want := range map[string]string{
			"Referrer-Policy":            "no-referrer",
			"Cache-Control":              "no-store",
			"X-Frame-Options":            "DENY",
			"X-Content-Type-Options":     "nosniff",
			"Cross-Origin-Opener-Policy": "same-origin",
		} {
			if got := h.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
		// A link to one of these pages carries authority in its fragment. A
		// browser that will fetch the page over plain http once has been given
		// a chance to have it read.
		if !strings.Contains(h.Get("Strict-Transport-Security"), "max-age=") {
			t.Errorf("%s: no Strict-Transport-Security", path)
		}
	}
}

// TestEveryScriptAPageNamesIsServed: a page that names a script this handler
// will not serve throws rather than refusing to sign.
func TestEveryScriptAPageNamesIsServed(t *testing.T) {
	for _, page := range []string{"/confirm", "/enroll", "/link", "/approve", "/reclaim", "/call"} {
		body := get(t, page).Body.String()
		rest := body
		found := 0
		for {
			const open = `<script src="`
			i := strings.Index(rest, open)
			if i < 0 {
				break
			}
			rest = rest[i+len(open):]
			src := rest[:strings.Index(rest, `"`)]
			found++
			if rec := get(t, src); rec.Code != http.StatusOK {
				t.Errorf("%s loads %s, which is served as %d", page, src, rec.Code)
			}
		}
		if found == 0 {
			t.Errorf("%s loads no script at all", page)
		}
	}
}

// TestTheSuitesAreNotHandedOut: the tests for these modules live beside them,
// and embedding the directory embeds them too. Nothing a page loads is ever
// named like one.
func TestTheSuitesAreNotHandedOut(t *testing.T) {
	entries, err := assets.ReadDir("static")
	if err != nil {
		t.Fatalf("read embedded assets: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".test.js") {
			continue
		}
		checked++
		if rec := get(t, "/static/"+e.Name()); rec.Code != http.StatusNotFound {
			t.Errorf("GET /static/%s = %d, want 404", e.Name(), rec.Code)
		}
	}
	if checked == 0 {
		t.Fatal("found no test files beside the pages; this test is checking nothing")
	}
}
