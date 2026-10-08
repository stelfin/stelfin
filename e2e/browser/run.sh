#!/bin/sh
# Runs the end-to-end test, holds its state open, and walks the signing pages
# through it in headless Chrome. See walk.mjs for why the pages need their own
# pass. Used by `make e2e-browser`.
set -eu

CHROME=${CHROME:-google-chrome}
PORT=${E2E_DEVTOOLS_PORT:-9333}
GO=${GO:-go}
NODE=${NODE:-node}

command -v "$CHROME" >/dev/null || { echo "no $CHROME on PATH; set CHROME=" >&2; exit 2; }

work=$(mktemp -d)
hold="$work/hold.txt"

"$CHROME" --headless=new --remote-debugging-port="$PORT" --user-data-dir="$work/profile" \
	--no-first-run --window-size=900,1400 about:blank >"$work/chrome.log" 2>&1 &
chrome=$!

E2E_HOLD=${E2E_HOLD:-15m} E2E_HOLD_FILE="$hold" \
	"$GO" test -tags=e2e ./e2e/ -count=1 -timeout 40m >"$work/e2e.log" 2>&1 &
test=$!

cleanup() {
	# Tells the held test it can stop waiting, then lets it shut its server and
	# its database down properly rather than killing them out from under it.
	touch "$hold.done"
	wait "$test" 2>/dev/null || true
	kill "$chrome" 2>/dev/null || true
	wait "$chrome" 2>/dev/null || true
	rm -rf "$work/profile" 2>/dev/null || true
}
trap cleanup EXIT

status=0
"$NODE" "$(dirname "$0")/walk.mjs" "$hold" "$PORT" || status=$?
if [ "$status" -ne 0 ] && [ -f "$hold.server.log" ]; then
	echo "what the server logged while the pages were walked:" >&2
	grep -E '"level":"(ERROR|WARN)"' "$hold.server.log" | grep -v 'webhook delivery refused' | tail -15 >&2 || true
fi

touch "$hold.done"
if ! wait "$test"; then
	echo "the end-to-end test itself failed:" >&2
	grep -E '^\s+e2e_test.go|^--- |^FAIL|"level":"ERROR"' "$work/e2e.log" >&2 || tail -20 "$work/e2e.log" >&2
	status=1
fi
exit $status
