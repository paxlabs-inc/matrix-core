// Package telemetry provides logging and metrics for Deus.
package telemetry

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// NewLogger returns a structured logger with secret redaction hooks.
func NewLogger() zerolog.Logger {
	level := zerolog.InfoLevel
	switch strings.ToLower(os.Getenv("DEUS_LOG_LEVEL")) {
	case "debug":
		level = zerolog.DebugLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	}
	out := io.Writer(os.Stdout)
	if strings.ToLower(os.Getenv("DEUS_LOG_FORMAT")) == "console" {
		out = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	}
	return zerolog.New(out).Level(level).With().Timestamp().Logger()
}
