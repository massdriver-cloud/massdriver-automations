// Package automation runs a Massdriver automation: an HTTP service that receives signed events from an
// HTTP event destination and acts on them.
package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// HookPath receives events from the Massdriver event destination.
const HookPath = "/hooks/massdriver"

const maxBodyBytes = 1 << 20

// Event is one CloudEvent from Massdriver.
type Event struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Source  string    `json:"source"`
	Subject string    `json:"subject"`
	Time    time.Time `json:"time"`
	Data    EventData `json:"data"`

	// Raw is the request body exactly as Massdriver signed it.
	Raw []byte `json:"-"`
}

// EventData is the `data` of an event. Entity and Actor differ by event type, so they stay raw.
type EventData struct {
	Entity     json.RawMessage `json:"entity"`
	Actor      json.RawMessage `json:"actor"`
	Attributes map[string]any  `json:"attributes"`
	Metadata   map[string]any  `json:"metadata"`
}

// Attribute returns an attribute of the event's entity as a string, or "" when it is absent.
func (e Event) Attribute(key string) string {
	v, ok := e.Data.Attributes[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Metadata returns a metadata field of the event as a string, or "" when it is absent.
func (e Event) Metadata(key string) string {
	v, ok := e.Data.Metadata[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// DecodeEntity decodes `data.entity` into v.
func (e Event) DecodeEntity(v any) error { return json.Unmarshal(e.Data.Entity, v) }

// DecodeActor decodes `data.actor` into v. Some event types have no actor.
func (e Event) DecodeActor(v any) error {
	if len(e.Data.Actor) == 0 {
		return errors.New("event has no actor")
	}
	return json.Unmarshal(e.Data.Actor, v)
}

// Automation handles events.
//
// Handle runs once for each verified event. Return nil when the event is handled or ignored. Return an error to
// respond 500, so Massdriver retries the event. Massdriver waits 10 seconds for a response: start long work in a
// goroutine and return nil.
type Automation interface {
	Handle(ctx context.Context, ev Event) error
}

// Router is implemented by an automation that serves more routes, such as a web page or a callback from a
// third-party service. Routes must not use HookPath or /healthz.
type Router interface {
	Routes(mux *http.ServeMux)
}

// Definition registers an automation with the runner.
type Definition struct {
	Name        string
	Description string
	// Events are the event types that the destination selects. Shown by `list`.
	Events []string
	New    func() (Automation, error)
	// Test is optional. It runs the automation's config against local input, such as a saved file, without
	// Massdriver, for `massdriver-automations test <name>`.
	Test func(args []string, stdin io.Reader, stdout io.Writer) error
}

// Handler returns the HTTP handler for a. It is separate from Serve so tests can drive it.
func Handler(a Automation, signingSecret []byte) http.Handler {
	return newHandler(a, signingSecret, nil)
}

// newHandler logs with base, or with slog.Default() at request time when base is nil.
func newHandler(a Automation, signingSecret []byte, base *slog.Logger) http.Handler {
	seen := newDedupe(time.Hour)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST "+HookPath, func(w http.ResponseWriter, r *http.Request) {
		log := Logger(r.Context())
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				log.Warn("request body too large", "limit_bytes", maxBodyBytes)
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			log.Warn("request body read failed", "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := VerifySignature(signingSecret, r.Header, body, time.Now()); err != nil {
			log.Warn("signature rejected", "reason", err.Error())
			// Massdriver does not retry a 401.
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}

		var ev Event
		if err := json.Unmarshal(body, &ev); err != nil {
			log.Warn("event decode failed", "err", err)
			http.Error(w, "bad event", http.StatusBadRequest)
			return
		}
		ev.Raw = body

		log = log.With("event_id", ev.ID, "event_type", ev.Type)
		recordEvent(r.Context(), ev)
		if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
			info.eventID, info.eventType = ev.ID, ev.Type
		}

		// Massdriver delivers at least once. A repeat of a handled event must not repeat its side effects.
		if seen.has(ev.ID) {
			log.Debug("duplicate event")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := a.Handle(WithLogger(r.Context(), log), ev); err != nil {
			log.Error("event handler failed", "err", err)
			recordError(r.Context(), err)
			http.Error(w, "handler failed", http.StatusInternalServerError)
			return
		}
		seen.add(ev.ID)
		w.WriteHeader(http.StatusNoContent)
	})

	if router, ok := a.(Router); ok {
		router.Routes(mux)
	}
	return withTracing(withAccessLog(withRecovery(mux), base), mux)
}

// Env returns the environment variable name, or fallback when it is unset or empty.
func Env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// RequireEnv returns the values of the named environment variables, or one error that names every missing one.
func RequireEnv(names ...string) (map[string]string, error) {
	values := make(map[string]string, len(names))
	var missing []string
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			missing = append(missing, n)
		}
		values[n] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %v", missing)
	}
	return values, nil
}
