package app

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/stelfin/stelfin/chat"
)

// platforms is the part of the transport registry that talks to the chat
// platforms at start. *chat.Registry implements it.
type platforms interface {
	Start(ctx context.Context) error
	RegisterCommands(ctx context.Context, cmds []chat.Command) error
}

// Bounds on how long to wait before asking a platform again.
const (
	prepareBackoffMin = 2 * time.Second
	prepareBackoffMax = 5 * time.Minute
)

// prepareTransports starts every transport and publishes the command list,
// and keeps trying until both have happened or ctx ends.
//
// It runs beside the server rather than before it. These are calls to somebody
// else's API, three or four of them, each allowed fifteen seconds, and none of
// them is a reason to keep a health check waiting or a signing page unserved —
// least of all on an instance that has just been woken by the request it is
// about to answer.
//
// It retries because one of the two matters. The command list is a menu, and a
// stale menu is an annoyance. But until Telegram's transport has been told who
// it is, it treats nothing said in a group as addressed to it, and a platform
// having a bad minute at boot should cost a minute of that, not a restart.
func prepareTransports(
	ctx context.Context, p platforms, cmds []chat.Command, backoff time.Duration, log *slog.Logger,
) {
	var started, published bool
	for {
		if !started {
			if err := p.Start(ctx); err != nil {
				log.Warn("a chat platform is not ready; commands still work, "+
					"but words in a group are not read until it is", "error", err)
			} else {
				started = true
			}
		}
		if !published {
			if err := p.RegisterCommands(ctx, cmds); err != nil {
				log.Warn("could not publish the command list; the bot still works, "+
					"but autocomplete may be stale", "error", err)
			} else {
				published = true
			}
		}
		if started && published {
			log.Info("chat platforms ready")
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > prepareBackoffMax {
			backoff = prepareBackoffMax
		}
	}
}

// telegramWebhookURL is where Telegram should deliver, or "" when it could not
// reach this server anyway.
//
// A deployment on a laptop is given no webhook. Telegram refuses a plain-http
// address outright, and a bot token shared between a laptop and a deployment
// would otherwise have each one take the other's deliveries away every time it
// started.
func telegramWebhookURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return strings.TrimSuffix(baseURL, "/") + "/webhook/telegram"
}
