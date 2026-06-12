// Package telemetry provides chronosd's structured logger. A thin wrapper over
// log/slog (JSON to stdout) so the rest of the codebase has one logging story.
package telemetry

import (
	"log/slog"
	"os"
)

// NewLogger returns a JSON structured logger at info level.
func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
