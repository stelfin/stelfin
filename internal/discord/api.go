package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultAPIURL is Discord's REST root.
const DefaultAPIURL = "https://discord.com/api/v10"

// callTimeout bounds one REST call.
//
// These happen after the interaction has been acknowledged, on a goroutine with
// its own deadline. Short enough that a wedged call does not consume that
// budget, long enough that an ordinary followup over a slow link still lands.
const callTimeout = 15 * time.Second

// maxResponseBytes bounds a REST response read.
const maxResponseBytes = 1 << 20

// apiError is a Discord-reported failure.
type apiError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("discord: %s %s: http %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// expired reports a followup rejected because the interaction token is no
// longer usable.
//
// Discord answers an unknown or expired interaction webhook with 404, and 401
// when the token is not accepted at all. Both mean the same thing to a caller:
// this route is gone, decide what to do instead. Fifteen minutes after the
// interaction, every followup lands here.
func (e *apiError) expired() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusUnauthorized
}

// call invokes one REST method.
//
// authenticate selects whether the bot token is sent. Interaction followups
// must not carry it: the interaction token in the path is the credential, and
// Discord rejects a bot-authenticated followup. Everything else needs it.
func (d *Transport) call(ctx context.Context, method, path string, body any, authenticate bool) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("discord: encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, d.apiURL+path, reader)
	if err != nil {
		return fmt.Errorf("discord: build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authenticate {
		req.Header.Set("Authorization", "Bot "+d.botToken)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("discord: %s %s: request failed", method, path)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("discord: read %s %s response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	return nil
}
