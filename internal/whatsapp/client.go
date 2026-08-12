// Package whatsapp sends messages through Meta's WhatsApp Cloud API.
package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultGraphURL is Meta's Graph API base. The version is pinned rather than
// tracking "latest": a silently changed payload shape in a payments path is
// exactly the kind of surprise worth taking a deliberate upgrade for.
const DefaultGraphURL = "https://graph.facebook.com/v21.0"

// Client sends WhatsApp messages.
type Client struct {
	http          *http.Client
	graphURL      string
	phoneNumberID string
	accessToken   string
}

// Config describes a Client.
type Config struct {
	// PhoneNumberID is the sending number's Meta id.
	PhoneNumberID string
	// AccessToken authenticates to the Graph API.
	AccessToken string
	// GraphURL overrides the API base. Empty uses DefaultGraphURL.
	GraphURL string
	// HTTPClient overrides the transport. Nil uses one with a timeout.
	HTTPClient *http.Client
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.PhoneNumberID == "" {
		return nil, fmt.Errorf("whatsapp: phone number id is required")
	}
	if cfg.AccessToken == "" {
		return nil, fmt.Errorf("whatsapp: access token is required")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		// A bounded timeout, because this runs inside the window a webhook
		// delivery is allowed to occupy.
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	graphURL := cfg.GraphURL
	if graphURL == "" {
		graphURL = DefaultGraphURL
	}

	return &Client{
		http:          httpClient,
		graphURL:      strings.TrimSuffix(graphURL, "/"),
		phoneNumberID: cfg.PhoneNumberID,
		accessToken:   cfg.AccessToken,
	}, nil
}

type sendRequest struct {
	MessagingProduct string `json:"messaging_product"`
	RecipientType    string `json:"recipient_type"`
	To               string `json:"to"`
	Type             string `json:"type"`
	Text             struct {
		Body string `json:"body"`
		// Link previews are disabled. A confirmation message carries a link
		// that authorises a payment, and having Meta fetch it server-side both
		// leaks the URL to a third party and could consume a single-use token.
		PreviewURL bool `json:"preview_url"`
	} `json:"text"`
}

// Send delivers a text message.
//
// to is an E.164 number; Meta accepts it with or without the leading '+'.
func (c *Client) Send(ctx context.Context, to, body string) error {
	if to == "" || body == "" {
		return fmt.Errorf("whatsapp: recipient and body are required")
	}

	var req sendRequest
	req.MessagingProduct = "whatsapp"
	req.RecipientType = "individual"
	req.To = strings.TrimPrefix(to, "+")
	req.Type = "text"
	req.Text.Body = body
	req.Text.PreviewURL = false

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("whatsapp: encode request: %w", err)
	}

	url := fmt.Sprintf("%s/%s/messages", c.graphURL, c.phoneNumberID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("whatsapp: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("whatsapp: send to %s: %w", redact(to), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a bounded amount of the error body: Meta's messages are useful
		// for diagnosis and there is no reason to accept an unbounded one.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("whatsapp: send to %s returned %d: %s",
			redact(to), resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

// redact keeps a phone number out of logs while leaving enough to correlate.
//
// A full number in an error string ends up in log aggregation, crash reports,
// and support tickets. The last four digits are enough to match against a user
// who is already in front of you.
func redact(number string) string {
	trimmed := strings.TrimPrefix(number, "+")
	if len(trimmed) <= 4 {
		return "***"
	}
	return "***" + trimmed[len(trimmed)-4:]
}
