// Package costexplorer serves a dashboard of the cloud costs of every instance, grouped and filtered by instance
// attributes, with a daily history.
package costexplorer

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const pageLimit = 100

var (
	//go:embed queries/instances.graphql
	instancesQuery string
	//go:embed dashboard.html
	dashboardHTML []byte
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "cost-explorer",
	Description: "Serves a dashboard of instance costs that you can group, filter, and chart.",
	Events:      []string{"deployment.completed"},
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		password := os.Getenv("COST_EXPLORER_PASSWORD")
		if !cfg.Public && password == "" {
			return nil, fmt.Errorf("set COST_EXPLORER_PASSWORD, or set public: true in the config")
		}
		e, err := New(api, cfg, password, os.Getenv("STATE_DIR"))
		if err != nil {
			return nil, err
		}
		go e.Loop(context.Background())
		return e, nil
	},
}

// Explorer holds the latest costs and the daily history.
type Explorer struct {
	api      *massdriver.Client
	cfg      Config
	password string
	stateDir string
	now      func() time.Time
	refresh  chan struct{}

	mu        sync.Mutex
	rows      []Row
	hist      history
	updatedAt time.Time
}

// Row is one instance and its costs. A nil amount means Massdriver has no billing data for that period.
type Row struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Status         string            `json:"status"`
	Dimensions     map[string]string `json:"dimensions"`
	LastDay        *float64          `json:"last_day"`
	DailyAverage   *float64          `json:"daily_average"`
	LastMonth      *float64          `json:"last_month"`
	MonthlyAverage *float64          `json:"monthly_average"`
	Currency       string            `json:"currency,omitempty"`
	URL            string            `json:"url,omitempty"`
}

// New returns the explorer with its history loaded from stateDir. It has no costs until Refresh.
func New(api *massdriver.Client, cfg Config, password, stateDir string) (*Explorer, error) {
	h, err := loadHistory(stateDir)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return &Explorer{
		api: api, cfg: cfg, password: password, stateDir: stateDir, now: time.Now,
		refresh: make(chan struct{}, 1), hist: h,
	}, nil
}

// Loop refreshes at start, every refresh interval, and soon after a deployment completes.
func (e *Explorer) Loop(ctx context.Context) {
	tick := time.NewTicker(time.Duration(e.cfg.Refresh))
	defer tick.Stop()
	e.refreshLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-e.refresh:
			// Deployments come in bursts. One read covers the burst.
			time.Sleep(30 * time.Second)
			select {
			case <-e.refresh:
			default:
			}
		}
		e.refreshLogged(ctx)
	}
}

// Handle asks for a refresh after a provision or decommission, which adds or removes an instance's costs.
func (e *Explorer) Handle(_ context.Context, ev automation.Event) error {
	action := ev.Metadata("deployment_action")
	if ev.Type != "deployment.completed" || (action != "PROVISION" && action != "DECOMMISSION") {
		return nil
	}
	select {
	case e.refresh <- struct{}{}:
	default:
	}
	return nil
}

func (e *Explorer) refreshLogged(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	start := time.Now()
	if err := e.Refresh(ctx); err != nil {
		automation.Logger(ctx).Error("cost refresh failed", "err", err)
		return
	}
	e.mu.Lock()
	n := len(e.rows)
	e.mu.Unlock()
	automation.Logger(ctx).Info("costs refreshed", "instances", n,
		"duration_ms", float64(time.Since(start).Microseconds())/1000)
}

type sample struct {
	Amount   *float64 `json:"amount"`
	Currency *string  `json:"currency"`
}

// Refresh reads the costs of every instance, and records today's daily costs in the history.
func (e *Explorer) Refresh(ctx context.Context) error {
	var rows []Row
	var next *string
	for {
		var out struct {
			Instances struct {
				Cursor struct {
					Next *string `json:"next"`
				} `json:"cursor"`
				Items []struct {
					ID                  string         `json:"id"`
					Name                string         `json:"name"`
					Status              string         `json:"status"`
					EffectiveAttributes map[string]any `json:"effectiveAttributes"`
					Bundle              *struct {
						Name string `json:"name"`
					} `json:"bundle"`
					Cost struct {
						LastDay        sample `json:"lastDay"`
						DailyAverage   sample `json:"dailyAverage"`
						LastMonth      sample `json:"lastMonth"`
						MonthlyAverage sample `json:"monthlyAverage"`
					} `json:"cost"`
				} `json:"items"`
			} `json:"instances"`
		}
		err := e.api.Do(ctx, instancesQuery, map[string]any{
			"org": e.api.OrganizationID, "cursor": map[string]any{"limit": pageLimit, "next": next},
		}, &out)
		if err != nil {
			return err
		}
		for _, it := range out.Instances.Items {
			attrs := map[string]string{}
			for k, v := range it.EffectiveAttributes {
				if v != nil {
					attrs[k] = fmt.Sprint(v)
				}
			}
			if it.Bundle != nil {
				attrs["bundle"] = it.Bundle.Name
			}
			r := Row{
				ID: it.ID, Name: it.Name, Status: it.Status, Dimensions: map[string]string{},
				LastDay: it.Cost.LastDay.Amount, DailyAverage: it.Cost.DailyAverage.Amount,
				LastMonth: it.Cost.LastMonth.Amount, MonthlyAverage: it.Cost.MonthlyAverage.Amount,
				URL: e.instanceURL(attrs),
			}
			// Only the configured dimensions leave the server: attributes can name things the viewer should not see.
			for _, d := range e.cfg.Dimensions {
				if v, ok := attrs[d]; ok {
					r.Dimensions[d] = v
				}
			}
			for _, s := range []sample{it.Cost.LastMonth, it.Cost.MonthlyAverage, it.Cost.DailyAverage, it.Cost.LastDay} {
				if s.Currency != nil && *s.Currency != "" {
					r.Currency = *s.Currency
					break
				}
			}
			rows = append(rows, r)
		}
		if next = out.Instances.Cursor.Next; next == nil {
			break
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	now := e.now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rows = rows
	e.updatedAt = now
	e.hist.record(now, rows, e.cfg.HistoryDays)
	return saveHistory(e.stateDir, e.hist)
}

func (e *Explorer) instanceURL(attrs map[string]string) string {
	p, env, c := attrs["md-project"], attrs["md-environment"], attrs["md-component"]
	if p == "" || env == "" || c == "" {
		return ""
	}
	return fmt.Sprintf("%s/orgs/%s/projects/%s/environments/%s/instances/%s", e.cfg.Massdriver.AppURL,
		url.PathEscape(e.api.OrganizationID), url.PathEscape(p), url.PathEscape(env), url.PathEscape(c))
}

// snapshot is the body of /api/costs.json.
type snapshot struct {
	Title      string     `json:"title"`
	Dimensions []string   `json:"dimensions"`
	UpdatedAt  *time.Time `json:"updated_at"`
	Instances  []Row      `json:"instances"`
	History    []day      `json:"history"`
	// HistoryInstances names and groups the instances in History, including deleted ones.
	HistoryInstances map[string]known `json:"history_instances"`
}

// Routes serves the dashboard at / and its data at /api/costs.json.
func (e *Explorer) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", e.auth(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("content-security-policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:")
		_, _ = w.Write(dashboardHTML)
	}))
	mux.HandleFunc("GET /api/costs.json", e.auth(func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		s := snapshot{
			Title: e.cfg.Title, Dimensions: e.cfg.Dimensions, Instances: e.rows,
			History: e.hist.Days, HistoryInstances: e.hist.Instances,
		}
		if !e.updatedAt.IsZero() {
			t := e.updatedAt
			s.UpdatedAt = &t
		}
		b, err := json.Marshal(s)
		e.mu.Unlock()
		if err != nil {
			http.Error(w, "encode failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Header().Set("cache-control", "no-cache")
		_, _ = w.Write(b)
	}))
}

func (e *Explorer) auth(next http.HandlerFunc) http.HandlerFunc {
	if e.cfg.Public {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(e.cfg.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(e.password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("www-authenticate", `Basic realm="cost-explorer", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
