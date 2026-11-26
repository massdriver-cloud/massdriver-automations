package automation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	// Massdriver waits 10 seconds for a webhook response. The status page renders from memory. Both finish well
	// inside this.
	writeTimeout = 30 * time.Second
	idleTimeout  = 120 * time.Second
	// ECS and Kubernetes both wait 30 seconds after SIGTERM before SIGKILL.
	shutdownTimeout = 20 * time.Second
)

// Serve verifies and dedupes events for a and serves them on addr. When ctx is done, it stops accepting
// connections and waits for in-flight requests to finish before it returns.
func Serve(ctx context.Context, addr string, a Automation) error {
	secret, err := SigningSecretFromEnv()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log := slog.Default()
	return serve(ctx, ln, newServer(Handler(a, secret), log), log, shutdownTimeout)
}

func newServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

func serve(ctx context.Context, ln net.Listener, srv *http.Server, log *slog.Logger, timeout time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout_ms", timeout.Milliseconds())
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}

type requestInfoKey struct{}

// requestInfo carries what the webhook handler learns about a request back out to the access log.
type requestInfo struct {
	eventID, eventType string
}

// withAccessLog logs one line per request. It never logs bodies, headers, or the query string: they carry
// signatures, tokens, and Slack payloads.
func withAccessLog(next http.Handler, base *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log := base
		if log == nil {
			log = slog.Default()
		}
		info := &requestInfo{}
		ctx := WithLogger(context.WithValue(r.Context(), requestInfoKey{}, info), log)
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(ctx))

		level := slog.LevelInfo
		if r.URL.Path == "/healthz" {
			level = slog.LevelDebug
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			statusAttr(rec.statusCode()),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.Int64("bytes", rec.bytes),
		}
		if info.eventID != "" {
			attrs = append(attrs, slog.String("event_id", info.eventID), slog.String("event_type", info.eventType))
		}
		Logger(ctx).LogAttrs(ctx, level, "request", attrs...)
	})
}

func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// net/http uses this panic to abort a response on purpose, and suppresses its log.
			if v == http.ErrAbortHandler {
				panic(v)
			}
			log := Logger(r.Context())
			if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok && info.eventID != "" {
				log = log.With("event_id", info.eventID, "event_type", info.eventType)
			}
			log.Error("panic recovered", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			if rec, ok := w.(*responseRecorder); ok && rec.status != 0 {
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Unwrap lets http.ResponseController reach the connection's own writer.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
