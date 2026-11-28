package oncall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// Responder is one person on call. Level orders them: 1 is paged first.
type Responder struct {
	Level     int
	Name      string
	Email     string
	AvatarURL string
	// Handle is the person's account in the provider, such as pagerduty:PXPGF42.
	Handle string
	// key tells people apart across the sources of one route.
	key string
}

// Provider reads who is on call now.
type Provider interface {
	OnCall(ctx context.Context, r Route) ([]Responder, error)
}

// maxPages bounds the pages of one PagerDuty query. Each has up to 100 on-call entries.
const maxPages = 10

// PagerDuty calls the PagerDuty REST API v2.
type PagerDuty struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// OnCall lists the earliest on-call entry of each user, level, and policy for the route's schedules, then for its
// escalation policies. PagerDuty joins filters of different kinds with AND, so each kind is its own query.
func (p *PagerDuty) OnCall(ctx context.Context, r Route) ([]Responder, error) {
	var out []Responder
	for _, q := range []struct {
		param string
		ids   []string
	}{{"schedule_ids[]", r.PagerDuty.Schedules}, {"escalation_policy_ids[]", r.PagerDuty.EscalationPolicies}} {
		if len(q.ids) == 0 {
			continue
		}
		got, err := p.list(ctx, q.param, q.ids)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func (p *PagerDuty) list(ctx context.Context, param string, ids []string) ([]Responder, error) {
	var out []Responder
	offset := 0
	for range maxPages {
		v := url.Values{"include[]": {"users"}, "earliest": {"true"}, "limit": {"100"},
			"offset": {strconv.Itoa(offset)}}
		v[param] = ids
		var page struct {
			OnCalls []struct {
				EscalationLevel int `json:"escalation_level"`
				User            struct {
					ID        string `json:"id"`
					Summary   string `json:"summary"`
					Name      string `json:"name"`
					Email     string `json:"email"`
					AvatarURL string `json:"avatar_url"`
				} `json:"user"`
			} `json:"oncalls"`
			More bool `json:"more"`
		}
		err := getJSON(ctx, p.HTTP, p.URL+"/oncalls?"+v.Encode(), map[string]string{
			"authorization": "Token token=" + p.Token, "accept": "application/vnd.pagerduty+json;version=2",
		}, "pagerduty", &page)
		if err != nil {
			return nil, err
		}
		for _, o := range page.OnCalls {
			u := o.User
			if u.ID == "" {
				continue
			}
			name := u.Name
			if name == "" {
				name = u.Summary
			}
			out = append(out, Responder{Level: o.EscalationLevel, Name: name, Email: u.Email, AvatarURL: u.AvatarURL,
				Handle: "pagerduty:" + u.ID, key: u.ID})
		}
		if !page.More || len(page.OnCalls) == 0 {
			break
		}
		offset += len(page.OnCalls)
	}
	return out, nil
}

// IncidentIO calls the incident.io API v2.
type IncidentIO struct {
	URL   string
	Token string
	HTTP  *http.Client
}

type incidentIOUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// OnCall lists the current shifts of the route's schedules, then the current responders of its escalation paths.
// incident.io has no levels across them, so each schedule or path is one level, in the order of the config.
func (c *IncidentIO) OnCall(ctx context.Context, r Route) ([]Responder, error) {
	headers := map[string]string{"authorization": "Bearer " + c.Token, "accept": "application/json"}
	var out []Responder
	level := 0
	add := func(users []incidentIOUser) {
		level++
		for _, u := range users {
			if u.ID != "" {
				out = append(out, Responder{Level: level, Name: u.Name, Email: u.Email, Handle: "incidentio:" + u.ID,
					key: u.ID})
			}
		}
	}
	for _, id := range r.IncidentIO.Schedules {
		var s struct {
			Schedule struct {
				CurrentShifts []struct {
					User incidentIOUser `json:"user"`
				} `json:"current_shifts"`
			} `json:"schedule"`
		}
		if err := getJSON(ctx, c.HTTP, c.URL+"/v2/schedules/"+url.PathEscape(id), headers, "incident.io", &s); err != nil {
			return nil, err
		}
		users := make([]incidentIOUser, len(s.Schedule.CurrentShifts))
		for i, sh := range s.Schedule.CurrentShifts {
			users[i] = sh.User
		}
		add(users)
	}
	for _, id := range r.IncidentIO.EscalationPaths {
		var p struct {
			EscalationPath struct {
				CurrentResponders []incidentIOUser `json:"current_responders"`
			} `json:"escalation_path"`
		}
		err := getJSON(ctx, c.HTTP, c.URL+"/v2/escalation_paths/"+url.PathEscape(id), headers, "incident.io", &p)
		if err != nil {
			return nil, err
		}
		add(p.EscalationPath.CurrentResponders)
	}
	return out, nil
}

func getJSON(ctx context.Context, client *http.Client, u string, headers map[string]string, service string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return automation.RedactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", service, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", service, err)
	}
	return nil
}
