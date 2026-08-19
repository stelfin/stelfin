package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultAPIURL is Telegram's Bot API root.
const DefaultAPIURL = "https://api.telegram.org"

// callTimeout bounds one Bot API call.
//
// Derived from what it is for, not copied from elsewhere: these calls happen
// after the webhook has already been acknowledged, on a goroutine whose own
// deadline bounds the whole message. Short enough that a wedged call does not
// hold that budget, long enough that an ordinary sendMessage over a slow link
// still lands.
const callTimeout = 15 * time.Second

// maxResponseBytes bounds a Bot API response read. Telegram's replies are
// small; anything large is a proxy or a captive portal, not Telegram.
const maxResponseBytes = 1 << 20

// apiError is a Telegram-reported failure.
type apiError struct {
	Method      string
	Code        int
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram: %s failed: %d %s", e.Method, e.Code, e.Description)
}

// envelope is the shape every Bot API response shares.
type envelope struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
}

// call invokes one Bot API method with a JSON body.
//
// The bot token is in the URL path, which is how Telegram's API works and is
// also why no error from this function may carry the URL: the path *is* the
// credential. Errors name the method instead.
func (t *Transport) call(ctx context.Context, method string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("telegram: encode %s: %w", method, err)
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	url := t.apiURL + "/bot" + t.token + "/" + method
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build %s request: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := t.http.Do(httpReq)
	if err != nil {
		// net/http puts the request URL in its error string, and that URL
		// carries the bot token. Report the method instead.
		return fmt.Errorf("telegram: %s: request failed", method)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("telegram: read %s response: %w", method, err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram: decode %s response (http %d): %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return &apiError{Method: method, Code: env.ErrorCode, Description: env.Description}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram: decode %s result: %w", method, err)
	}
	return nil
}
