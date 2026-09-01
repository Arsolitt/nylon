// Package logging provides the single logger constructor shared by all nylon
// binaries (nylon, nylon-lb, nylon-genesis). Every record carries component
// and (when set) node attributes so output from mixed deployments stays
// attributable.
package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/encodeous/tint"
	slogmulti "github.com/samber/slog-multi"
)

// EnvLogLevel is the environment variable consulted by Resolve.
const EnvLogLevel = "NYLON_LOG_LEVEL"

// Config describes the process root logger.
type Config struct {
	Component string // "nylon" | "nylon-lb" | "nylon-genesis"; required, used as tint prefix + attr
	Node      string // node id / hostname; "" omits the node attr
	Level     slog.Level
	JSON      bool   // stderr JSON handler instead of tint
	FilePath  string // optional file sink; JSON handler, file opened 0600, parent dirs 0700
}

// New returns the process root logger and a closer (closes the file sink; no-op otherwise).
func New(cfg Config) (*slog.Logger, func(), error) {
	if cfg.Component == "" {
		return nil, nil, errors.New("logging: component is required")
	}

	handlers := make([]slog.Handler, 0, 2)
	if cfg.JSON {
		handlers = append(handlers, slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level:     cfg.Level,
			AddSource: cfg.Level <= slog.LevelDebug,
		}))
	} else {
		handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
			Level:        cfg.Level,
			AddSource:    cfg.Level <= slog.LevelDebug,
			CustomPrefix: cfg.Component,
			ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
				if attr.Key == "time" {
					return slog.Attr{}
				}
				return attr
			},
		}))
	}

	closer := func() {}
	if cfg.FilePath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.FilePath), 0o700); err != nil {
			return nil, nil, fmt.Errorf("logging: create log dir: %w", err)
		}
		f, err := os.OpenFile(cfg.FilePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("logging: open log file: %w", err)
		}
		handlers = append(handlers, slog.NewJSONHandler(f, &slog.HandlerOptions{
			Level:     cfg.Level,
			AddSource: cfg.Level <= slog.LevelDebug,
		}))
		closer = func() { _ = f.Close() }
	}

	attrs := []any{"component", cfg.Component}
	if cfg.Node != "" {
		attrs = append(attrs, "node", cfg.Node)
	}

	logger := slog.New(slogmulti.Fanout(handlers...)).With(attrs...)
	return logger, closer, nil
}

// ParseLevel parses "debug"|"info"|"warn"|"error" case-insensitively.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level %q, valid values: debug, info, warn, error", s)
	}
}

// Resolve: non-empty flagLevel wins, then env NYLON_LOG_LEVEL, then verbose => Debug, else Info.
// Unknown value in either source returns an error naming valid values.
func Resolve(flagLevel string, verbose bool) (slog.Level, error) {
	if flagLevel != "" {
		return ParseLevel(flagLevel)
	}
	if env := os.Getenv(EnvLogLevel); env != "" {
		return ParseLevel(env)
	}
	if verbose {
		return slog.LevelDebug, nil
	}
	return slog.LevelInfo, nil
}
