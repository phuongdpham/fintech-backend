// Package logger is the project's structured-logging facade.
// It wraps slog (and optionally zap) so call sites use a single
// stable API while transport, sampling, and redaction policy
// stay swappable.
package logger

import (
	"context"
	"log/slog"
	"os"
)

type Logger struct {
	svc string
	l   *slog.Logger
}

func New(service string) *Logger {
	return &Logger{
		svc: service,
		l:   slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

func (l *Logger) Info(ctx context.Context, msg string, args ...any) {
	l.l.InfoContext(ctx, msg, append([]any{"svc", l.svc}, args...)...)
}
