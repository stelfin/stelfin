package app

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/connector/egress"
	"github.com/stelfin/stelfin/connector/sheets"
	"github.com/stelfin/stelfin/internal/config"
)

// connectorsFor builds what reaching a workspace's own tools needs, from
// whatever of it this deployment was given.
//
// Each half is optional and independent. A service account makes spreadsheets
// available; a connector key makes MCP servers and webhooks available, because
// those have an endpoint and a token to keep and nowhere to keep them without
// one. A deployment with neither returns nil, and every connector command says
// that connectors are not available here rather than failing later.
func connectorsFor(cfg *config.Config, log *slog.Logger) (*api.ConnectorConfig, error) {
	if len(cfg.ConnectorKey) == 0 && len(cfg.GoogleServiceAccount) == 0 {
		log.Warn("no connector key and no Google service account configured; " +
			"workspaces cannot register connectors")
		return nil, nil
	}

	out := &api.ConnectorConfig{}
	if len(cfg.ConnectorKey) > 0 {
		sealer, err := connector.NewSealer(cfg.ConnectorKey)
		if err != nil {
			return nil, err
		}
		out.Sealer = sealer
		// The one client every workspace-supplied address is dialled through.
		// It resolves a name once, refuses anything that is not a public
		// address, and connects to the address it checked.
		out.Egress = egress.NewClient(egress.Options{})
	}
	if len(cfg.GoogleServiceAccount) > 0 {
		account, err := sheets.NewServiceAccount(cfg.GoogleServiceAccount, nil)
		if err != nil {
			return nil, fmt.Errorf("app: Google service account: %w", err)
		}
		out.Sheets = account
		// Only the address, which is what a workspace shares its sheet with
		// and is not a secret. The constructor above has already accepted the
		// file, so this cannot fail on anything but a field it does not need.
		var public struct {
			ClientEmail string `json:"client_email"`
		}
		if err := json.Unmarshal(cfg.GoogleServiceAccount, &public); err == nil {
			out.SheetsAccount = public.ClientEmail
		}
	}

	log.Info("connectors configured",
		"sheets", out.Sheets != nil, "sheets_account", out.SheetsAccount,
		"mcp_and_webhook", out.Sealer != nil)
	return out, nil
}
