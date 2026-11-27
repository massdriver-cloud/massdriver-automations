package statuspage

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"fmt"
	"html/template"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	timeFormat     = "Jan 2, 2006 15:04 MST"
	recentDays     = 30
	pastIncidents  = 25
	feedIncidents  = 50
	badgeCharWidth = 6.5
)

var labels = map[string]string{
	operational:   "Operational",
	maintenance:   "Under maintenance",
	degraded:      "Degraded performance",
	partialOutage: "Partial outage",
	majorOutage:   "Major outage",
	noData:        "No data",
}

var headlines = map[string]string{
	operational:   "All systems operational",
	maintenance:   "Maintenance in progress",
	degraded:      "Degraded performance",
	partialOutage: "Partial outage",
	majorOutage:   "Major outage",
}

var colors = map[string]string{
	operational:   "#16a34a",
	maintenance:   "#2563eb",
	degraded:      "#ca8a04",
	partialOutage: "#ea580c",
	majorOutage:   "#dc2626",
	noData:        "#9ca3af",
}

func statusLabel(s string) string { return labels[s] }

type siteView struct {
	Title       string
	Logo        string
	Accent      template.CSS
	Timezone    string
	FooterLinks []Link
}

type navView struct {
	Title   string
	Href    string
	Current bool
}

type dayView struct {
	Status string
	Label  string
	Recent bool
}

type componentView struct {
	Title       string
	Description string
	Status      string
	StatusLabel string
	Uptime      string
	Days        []dayView
}

type envView struct {
	Title       string
	Description string
	Status      string
	StatusLabel string
	Components  []componentView
}

type updateView struct {
	At      string
	Status  string
	Message string
}

type incidentView struct {
	ID          string
	Title       string
	Environment string
	Impact      string
	ImpactLabel string
	Started     string
	Resolved    string
	Duration    string
	Updates     []updateView
}

type maintenanceView struct {
	Title       string
	Description string
	Window      string
	Scope       string
	Active      bool
}

type announcementView struct {
	Message string
	Level   string
	URL     string
	// Environment is the title of the environment whose md/v1/banner this is. Empty for config announcements.
	Environment string
}

// PageView is the data that the page template receives.
type PageView struct {
	Site            siteView
	Nav             []navView
	Title           string
	Description     string
	Status          string
	StatusLabel     string
	Headline        string
	Announcements   []announcementView
	Environments    []envView
	ActiveIncidents []incidentView
	PastIncidents   []incidentView
	Maintenance     []maintenanceView
	HistoryDays     int
	Home            string
	Updated         string
	FeedHref        string
	BadgeHref       string
	JSONHref        string
}

func (s *StatusPage) build(pages []pageState, now time.Time) (map[string]file, error) {
	files := map[string]file{}
	var status jsonStatus
	if !s.updatedAt.IsZero() {
		u := s.updatedAt.UTC()
		status.UpdatedAt = &u
	}

	for i, p := range pages {
		for _, root := range s.roots(i, p.Cfg.Slug) {
			var buf bytes.Buffer
			if err := s.tmpl.Execute(&buf, s.pageView(pages, i, root, now)); err != nil {
				return nil, err
			}
			files[root.dir+"index.html"] = file{"text/html; charset=utf-8", buf.Bytes()}
		}
		feed, err := s.feed(p, now)
		if err != nil {
			return nil, err
		}
		badge := s.badge(p.Status)
		files[p.Cfg.Slug+"/feed.atom"] = file{"application/atom+xml", feed}
		files[p.Cfg.Slug+"/badge.svg"] = file{"image/svg+xml", badge}
		if i == 0 {
			files["feed.atom"] = file{"application/atom+xml", feed}
			files["badge.svg"] = file{"image/svg+xml", badge}
		}
		status.Pages = append(status.Pages, s.jsonPage(p, now))
	}

	b, err := marshalJSON(status)
	if err != nil {
		return nil, err
	}
	files["api/status.json"] = file{"application/json", b}
	return files, nil
}

type root struct {
	dir    string
	prefix string
}

// Links are relative so the files work from any host or path prefix when copied out of output_dir.
func (s *StatusPage) roots(i int, slug string) []root {
	r := []root{{dir: slug + "/", prefix: "../"}}
	if i == 0 {
		r = append(r, root{dir: "", prefix: ""})
	}
	return r
}

func (s *StatusPage) pageView(pages []pageState, idx int, r root, now time.Time) PageView {
	p := pages[idx]
	loc := s.cfg.location
	v := PageView{
		Site: siteView{
			Title:       s.cfg.Site.Title,
			Logo:        s.cfg.Site.Logo,
			Accent:      template.CSS(s.cfg.Site.AccentColor), //nolint:gosec // Validate allows only a hex color.
			Timezone:    s.cfg.Site.Timezone,
			FooterLinks: s.cfg.Site.FooterLinks,
		},
		Title:       p.Cfg.Title,
		Description: p.Cfg.Description,
		Status:      p.Status,
		StatusLabel: statusLabel(p.Status),
		Headline:    headlines[p.Status],
		HistoryDays: s.cfg.HistoryDays,
		Home:        cmp.Or(r.prefix, "./"),
		FeedHref:    r.prefix + p.Cfg.Slug + "/feed.atom",
		BadgeHref:   r.prefix + p.Cfg.Slug + "/badge.svg",
		JSONHref:    r.prefix + "api/status.json",
	}
	if !s.updatedAt.IsZero() {
		v.Updated = s.updatedAt.In(loc).Format(timeFormat)
	}
	if len(pages) > 1 {
		for j, other := range pages {
			href := r.prefix + other.Cfg.Slug + "/"
			if j == 0 {
				href = cmp.Or(r.prefix, "./")
			}
			v.Nav = append(v.Nav, navView{Title: other.Cfg.Title, Href: href, Current: j == idx})
		}
	}

	for _, a := range s.cfg.Announcements {
		if (a.StartsAt != nil && now.Before(*a.StartsAt)) || (a.EndsAt != nil && !now.Before(*a.EndsAt)) {
			continue
		}
		if len(a.Pages) > 0 && !slices.Contains(a.Pages, p.Cfg.Slug) {
			continue
		}
		v.Announcements = append(v.Announcements, announcementView{Message: a.Message, Level: a.Level, URL: a.URL})
	}
	v.Announcements = append(v.Announcements, s.stashBanners(p, now)...)

	for _, e := range p.Envs {
		ev := envView{Title: e.Cfg.Title, Description: e.Cfg.Description, Status: e.Status, StatusLabel: statusLabel(e.Status)}
		for _, row := range e.Rows {
			days, uptime := s.days(row.Key, now)
			ev.Components = append(ev.Components, componentView{
				Title:       row.Title,
				Description: row.Description,
				Status:      row.Status,
				StatusLabel: statusLabel(row.Status),
				Uptime:      formatUptime(uptime),
				Days:        days,
			})
		}
		v.Environments = append(v.Environments, ev)
	}

	for _, inc := range s.pageIncidents(p.Cfg.Slug) {
		iv := s.incidentView(inc, now)
		if inc.ResolvedAt == nil {
			v.ActiveIncidents = append(v.ActiveIncidents, iv)
		} else if len(v.PastIncidents) < pastIncidents {
			v.PastIncidents = append(v.PastIncidents, iv)
		}
	}

	v.Maintenance = s.maintenanceViews(p, now)
	return v
}

// pageIncidents returns the page's incidents, newest first.
func (s *StatusPage) pageIncidents(slug string) []*incident {
	var out []*incident
	for _, inc := range s.hist.Incidents {
		if inc.Page == slug {
			out = append(out, inc)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

func (s *StatusPage) incidentView(inc *incident, now time.Time) incidentView {
	loc := s.cfg.location
	end := now
	iv := incidentView{
		ID:          inc.ID,
		Title:       inc.Title,
		Environment: inc.EnvTitle,
		Impact:      inc.Impact,
		ImpactLabel: statusLabel(inc.Impact),
		Started:     inc.StartedAt.In(loc).Format(timeFormat),
	}
	if inc.ResolvedAt != nil {
		end = *inc.ResolvedAt
		iv.Resolved = inc.ResolvedAt.In(loc).Format(timeFormat)
	}
	iv.Duration = formatDuration(end.Sub(inc.StartedAt))
	for i := len(inc.Updates) - 1; i >= 0; i-- {
		u := inc.Updates[i]
		iv.Updates = append(iv.Updates, updateView{At: u.At.In(loc).Format(timeFormat), Status: u.Status, Message: u.Message})
	}
	return iv
}

func (s *StatusPage) maintenanceViews(p pageState, now time.Time) []maintenanceView {
	loc := s.cfg.location
	titles := map[string]string{}
	for _, e := range p.Cfg.Environments {
		titles[e.ID] = e.Title
	}

	var out []maintenanceView
	var deploying []string
	for _, e := range p.Envs {
		for _, r := range e.Rows {
			if r.Deploying {
				deploying = append(deploying, e.Cfg.Title+" · "+r.Title)
			}
		}
	}
	if len(deploying) > 0 {
		out = append(out, maintenanceView{Title: "Deployment in progress", Scope: strings.Join(deploying, ", "), Active: true})
	}

	windows := slices.Clone(s.cfg.Maintenance)
	sort.SliceStable(windows, func(a, b int) bool { return windows[a].StartsAt.Before(windows[b].StartsAt) })
	for _, m := range windows {
		if !now.Before(m.EndsAt) {
			continue
		}
		var scope []string
		for _, id := range m.Environments {
			if t, ok := titles[id]; ok {
				scope = append(scope, t)
			}
		}
		if len(scope) == 0 {
			continue
		}
		if len(m.Components) > 0 {
			scope = []string{strings.Join(scope, ", ") + " · " + strings.Join(m.Components, ", ")}
		}
		out = append(out, maintenanceView{
			Title:       m.Title,
			Description: m.Description,
			Window:      m.StartsAt.In(loc).Format(timeFormat) + " to " + m.EndsAt.In(loc).Format(timeFormat),
			Scope:       strings.Join(scope, ", "),
			Active:      !now.Before(m.StartsAt),
		})
	}
	return out
}

// days returns one bar for each day of history_days, oldest first, and the uptime over the days with data.
// Partial and major outages count as downtime. Degraded performance and maintenance do not.
func (s *StatusPage) days(key string, now time.Time) ([]dayView, float64) {
	ts := s.hist.Rows[key]
	loc := s.cfg.location
	n := s.cfg.HistoryDays
	y, m, d := now.In(loc).Date()

	var down, observed time.Duration
	out := make([]dayView, n)
	for i := range n {
		start := time.Date(y, m, d-(n-1-i), 0, 0, 0, 0, loc)
		end := time.Date(y, m, d-(n-1-i)+1, 0, 0, 0, 0, loc)
		if end.After(now) {
			end = now
		}
		spent := map[string]time.Duration{}
		for k, t := range ts {
			segEnd := now
			if k+1 < len(ts) {
				segEnd = ts[k+1].At
			}
			from, to := maxTime(t.At, start), minTime(segEnd, end)
			if to.After(from) {
				spent[t.Status] += to.Sub(from)
			}
		}

		status := noData
		var dayTotal time.Duration
		for st, dur := range spent {
			dayTotal += dur
			if status == noData || rank[st] > rank[status] {
				status = st
			}
		}
		dayDown := spent[partialOutage] + spent[majorOutage]
		down += dayDown
		observed += dayTotal

		label := start.Format("Jan 2, 2006") + ": "
		switch {
		case status == noData:
			label += "no data"
		case dayDown > 0:
			label += strings.ToLower(statusLabel(status)) + ", " + formatDuration(dayDown) + " of downtime"
		case status == operational:
			label += "no incidents"
		default:
			label += strings.ToLower(statusLabel(status))
		}
		out[i] = dayView{Status: status, Label: label, Recent: i >= n-recentDays}
	}
	if observed == 0 {
		return out, -1
	}
	return out, 100 * (1 - float64(down)/float64(observed))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Truncate rather than round, so a page never shows 100.00% for a period that had downtime.
func formatUptime(pct float64) string {
	if pct < 0 {
		return ""
	}
	return fmt.Sprintf("%.2f%%", math.Floor(pct*100)/100)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "less than a minute"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}

// --- status.json -----------------------------------------------------------

type jsonStatus struct {
	UpdatedAt *time.Time `json:"updated_at"`
	Pages     []jsonPage `json:"pages"`
}

type jsonPage struct {
	Slug                 string            `json:"slug"`
	Title                string            `json:"title"`
	URL                  string            `json:"url,omitempty"`
	Status               string            `json:"status"`
	Environments         []jsonEnv         `json:"environments"`
	Incidents            []jsonIncident    `json:"incidents"`
	ScheduledMaintenance []jsonMaintenance `json:"scheduled_maintenance"`
}

type jsonEnv struct {
	Title      string          `json:"title"`
	Status     string          `json:"status"`
	Components []jsonComponent `json:"components"`
}

type jsonComponent struct {
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	UptimePercent *float64 `json:"uptime_percent"`
}

type jsonIncident struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Environment string       `json:"environment"`
	Impact      string       `json:"impact"`
	Status      string       `json:"status"`
	StartedAt   time.Time    `json:"started_at"`
	ResolvedAt  *time.Time   `json:"resolved_at"`
	Updates     []jsonUpdate `json:"updates"`
}

type jsonUpdate struct {
	At      time.Time `json:"at"`
	Status  string    `json:"status"`
	Message string    `json:"message"`
}

type jsonMaintenance struct {
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Active      bool      `json:"active"`
}

func (s *StatusPage) pageURL(slug string) string {
	if s.cfg.Site.BaseURL == "" {
		return ""
	}
	return s.cfg.Site.BaseURL + "/" + slug + "/"
}

func (s *StatusPage) jsonPage(p pageState, now time.Time) jsonPage {
	jp := jsonPage{
		Slug:                 p.Cfg.Slug,
		Title:                p.Cfg.Title,
		URL:                  s.pageURL(p.Cfg.Slug),
		Status:               p.Status,
		Environments:         []jsonEnv{},
		Incidents:            []jsonIncident{},
		ScheduledMaintenance: []jsonMaintenance{},
	}
	for _, e := range p.Envs {
		je := jsonEnv{Title: e.Cfg.Title, Status: e.Status, Components: []jsonComponent{}}
		for _, r := range e.Rows {
			jc := jsonComponent{Title: r.Title, Status: r.Status}
			if _, up := s.days(r.Key, now); up >= 0 {
				v := math.Floor(up*100) / 100
				jc.UptimePercent = &v
			}
			je.Components = append(je.Components, jc)
		}
		jp.Environments = append(jp.Environments, je)
	}
	for _, inc := range s.pageIncidents(p.Cfg.Slug) {
		ji := jsonIncident{ID: inc.ID, Title: inc.Title, Environment: inc.EnvTitle, Impact: inc.Impact, Status: "ongoing",
			StartedAt: inc.StartedAt.UTC(), Updates: []jsonUpdate{}}
		if inc.ResolvedAt != nil {
			r := inc.ResolvedAt.UTC()
			ji.Status, ji.ResolvedAt = "resolved", &r
		}
		for _, u := range inc.Updates {
			ji.Updates = append(ji.Updates, jsonUpdate{At: u.At.UTC(), Status: u.Status, Message: u.Message})
		}
		jp.Incidents = append(jp.Incidents, ji)
	}
	envs := map[string]bool{}
	for _, e := range p.Cfg.Environments {
		envs[e.ID] = true
	}
	for _, m := range s.cfg.Maintenance {
		if now.Before(m.EndsAt) && slices.ContainsFunc(m.Environments, func(id string) bool { return envs[id] }) {
			jp.ScheduledMaintenance = append(jp.ScheduledMaintenance, jsonMaintenance{
				Title: m.Title, Description: m.Description, StartsAt: m.StartsAt.UTC(), EndsAt: m.EndsAt.UTC(),
				Active: !now.Before(m.StartsAt),
			})
		}
	}
	return jp
}

// --- Atom feed -------------------------------------------------------------

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
}

type atomEntry struct {
	ID      string     `xml:"id"`
	Title   string     `xml:"title"`
	Updated string     `xml:"updated"`
	Links   []atomLink `xml:"link,omitempty"`
	Content atomText   `xml:"content"`
}

type atomText struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

func (s *StatusPage) feed(p pageState, now time.Time) ([]byte, error) {
	url := s.pageURL(p.Cfg.Slug)
	id := url
	if id == "" {
		id = "urn:statuspage:" + p.Cfg.Slug
	}
	f := atomFeed{ID: id, Title: s.cfg.Site.Title + " · " + p.Cfg.Title, Updated: now.UTC().Format(time.RFC3339)}
	if !s.updatedAt.IsZero() {
		f.Updated = s.updatedAt.UTC().Format(time.RFC3339)
	}
	if url != "" {
		f.Links = []atomLink{{Href: url}, {Href: url + "feed.atom", Rel: "self"}}
	}

	for _, inc := range s.pageIncidents(p.Cfg.Slug) {
		if len(f.Entries) == feedIncidents {
			break
		}
		last := inc.Updates[len(inc.Updates)-1]
		title := inc.EnvTitle + " · " + inc.Title
		if inc.ResolvedAt != nil {
			title += " (resolved)"
		}
		var body []string
		for i := len(inc.Updates) - 1; i >= 0; i-- {
			u := inc.Updates[i]
			body = append(body, u.At.In(s.cfg.location).Format(timeFormat)+": "+u.Message)
		}
		e := atomEntry{
			ID:      "urn:statuspage:incident:" + inc.ID,
			Title:   title,
			Updated: last.At.UTC().Format(time.RFC3339),
			Content: atomText{Type: "text", Body: strings.Join(body, "\n")},
		}
		if url != "" {
			e.Links = []atomLink{{Href: url + "#incident-" + inc.ID}}
		}
		f.Entries = append(f.Entries, e)
	}

	b, err := xml.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(b, '\n')...), nil
}

// --- badge -----------------------------------------------------------------

func (s *StatusPage) badge(status string) []byte {
	label, msg := "status", strings.ToLower(statusLabel(status))
	lw := int(float64(len(label))*badgeCharWidth) + 12
	mw := int(float64(len(msg))*badgeCharWidth) + 12
	w := lw + mw
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`, w, label, msg)
	fmt.Fprintf(&b, `<title>%s: %s</title>`, label, msg)
	fmt.Fprintf(&b, `<clipPath id="r"><rect width="%d" height="20" rx="3"/></clipPath><g clip-path="url(#r)">`, w)
	fmt.Fprintf(&b, `<rect width="%d" height="20" fill="#555"/><rect x="%d" width="%d" height="20" fill="%s"/></g>`, lw, lw, mw, colors[status])
	b.WriteString(`<g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11">`)
	fmt.Fprintf(&b, `<text x="%d" y="14">%s</text><text x="%d" y="14">%s</text></g></svg>`, lw/2, label, lw+mw/2, msg)
	b.WriteString("\n")
	return b.Bytes()
}
