// Command stelfind runs the stelfin server: the chat-platform webhooks, the
// confirmation API, and the Horizon ingestion worker.
package main

import (
	"log/slog"
	"os"

	"github.com/stelfin/stelfin/internal/app"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := app.Run(log); err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
}
