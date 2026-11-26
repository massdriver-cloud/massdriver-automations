package automation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/lmittmann/tint"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/term"
)

// NewLogger returns a logger that writes to w. format is "auto", "json", or "text"; level is "debug", "info", "warn",
// or "error". Both are case-insensitive. "auto" writes colored text when w is a terminal and JSON otherwise. "text"
// writes colored text unless NO_COLOR is set (https://no-color.org).
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

	switch strings.ToLower(format) {
	case "auto":
		if isTerminal(w) {
			return newTextLogger(w, lvl), nil
		}
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
	case "text":
		return newTextLogger(w, lvl), nil
	default:
		return nil, fmt.Errorf("LOG_FORMAT %q: want auto, json, or text", format)
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(f.Fd()))
}

// ANSI 256-color codes, as tint.Attr takes them.
const (
	colorGray   uint8 = 8
	colorRed    uint8 = 9
	colorGreen  uint8 = 10
	colorYellow uint8 = 11
)

func newTextLogger(w io.Writer, lvl slog.Level) *slog.Logger {
	return slog.New(tint.NewHandler(w, &tint.Options{
		Level:      lvl,
		TimeFormat: "15:04:05.000",
		NoColor:    os.Getenv("NO_COLOR") != "",
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Value.Kind() != slog.KindAny {
				return a
			}
			switch v := a.Value.Any().(type) {
			case slog.Level:
				if len(groups) == 0 && a.Key == slog.LevelKey && v < slog.LevelInfo {
					return tint.Attr(colorGray, a)
				}
			case error:
				return tint.Attr(colorRed, a)
			}
			return a
		},
	}))
}

// statusAttr is the HTTP status of a response, colored by class in text logs. JSON logs get the plain number.
func statusAttr(code int) slog.Attr {
	a := slog.Int("status", code)
	switch {
	case code >= 500:
		return tint.Attr(colorRed, a)
	case code >= 400:
		return tint.Attr(colorYellow, a)
	case code >= 200 && code < 300:
		return tint.Attr(colorGreen, a)
	}
	return a
}

type loggerKey struct{}

// WithLogger returns a copy of ctx that carries l. Logger(ctx) returns it.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	// The trace ids belong to the span in the context where Logger is called, not to the one where l was stored.
	if h, ok := l.Handler().(spanHandler); ok {
		l = slog.New(h.base)
	}
	return context.WithValue(ctx, loggerKey{}, l)
}

// Logger returns the logger for ctx. In Handle it carries event_id and event_type. Work that outlives the request
// keeps the logger when it starts from context.WithoutCancel(ctx). Without one, it is slog.Default(). When ctx has
// a span, its records also carry trace_id and span_id.
func Logger(ctx context.Context) *slog.Logger {
	l, ok := ctx.Value(loggerKey{}).(*slog.Logger)
	if !ok {
		l = slog.Default()
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return l
	}
	base := l.Handler()
	if h, ok := base.(spanHandler); ok {
		base = h.base
	}
	return slog.New(spanHandler{
		Handler: base.WithAttrs([]slog.Attr{
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		}),
		base: base,
	})
}

// spanHandler writes trace_id and span_id, and keeps the handler without them so WithLogger can store that one.
type spanHandler struct {
	slog.Handler
	base slog.Handler
}

func (h spanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return spanHandler{Handler: h.Handler.WithAttrs(attrs), base: h.base.WithAttrs(attrs)}
}

func (h spanHandler) WithGroup(name string) slog.Handler {
	return spanHandler{Handler: h.Handler.WithGroup(name), base: h.base.WithGroup(name)}
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
