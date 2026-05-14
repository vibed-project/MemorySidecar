package obs

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"memsidecar/internal/config"
)

// LoggerSetup is the result of NewLogger. The returned Level is mutable —
// callers can change the active log level at runtime by calling Level.Set,
// which is what the SIGHUP reload path does.
type LoggerSetup struct {
	Logger *slog.Logger
	Level  *slog.LevelVar
}

// NewLogger builds a logger backed by a LevelVar so the level can change at
// runtime.
func NewLogger(cfg config.LoggingConfig) (*LoggerSetup, error) {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	lv := new(slog.LevelVar)
	lv.Set(level)
	handlerOpts := &slog.HandlerOptions{Level: lv}

	var h slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "json", "":
		h = slog.NewJSONHandler(os.Stderr, handlerOpts)
	case "text":
		h = slog.NewTextHandler(os.Stderr, handlerOpts)
	default:
		return nil, fmt.Errorf("unknown log format %q", cfg.Format)
	}
	return &LoggerSetup{Logger: slog.New(h), Level: lv}, nil
}

// ParseLevel converts a string level into slog.Level. Empty string → Info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}
