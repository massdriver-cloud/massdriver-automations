package automation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
)

// NewLogger returns a logger that writes to w. format is "json" or "text"; level is "debug", "info", "warn", or
// "error". Both are case-insensitive.
func NewLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("LOG_LEVEL %q: want debug, info, warn, or error", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("LOG_FORMAT %q: want json or text", format)
	}
}

type loggerKey struct{}

// WithLogger returns a copy of ctx that carries l. Logger(ctx) returns it.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// Logger returns the logger for ctx. In Handle it carries event_id and event_type. Work that outlives the request
// keeps the logger when it starts from context.WithoutCancel(ctx). Without one, it is slog.Default().
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// RedactURL removes the path and query from the URL inside a *url.Error. The net/http client puts the full request
// URL in its errors, and a Slack or Teams webhook URL is a credential.
func RedactURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	u, perr := url.Parse(ue.URL)
	if perr != nil {
		return &url.Error{Op: ue.Op, URL: "[redacted]", Err: ue.Err}
	}
	return &url.Error{Op: ue.Op, URL: u.Scheme + "://" + u.Host, Err: ue.Err}
}
