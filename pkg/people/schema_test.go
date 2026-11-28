package people

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/massdriver-cloud/massdriver-automations/internal/widgetschema"
)

// Whatever a provider returns, Clean leaves a person the server accepts, or nothing.
func TestCleanedPeopleFitTheWidgetSchema(t *testing.T) {
	inputs := []Person{
		{Name: strings.Repeat("ü", 200), ImageURL: "https://x.test/" + strings.Repeat("a", 3000), Email: "a b@c"},
		{Name: "   ", ImageURL: "javascript:alert(1)", Email: "a@b.test, c@d.test", Handles: []string{"x"}},
		{Name: "Jo\n", Email: strings.Repeat("a", 250) + "@x.test", Handles: []string{"github:jo", "github: jo"}},
		{Handles: []string{"slack:@a", "slack:@b", "slack:@c", "slack:@d", "slack:@e", "slack:@f", "slack:@g",
			"slack:@h", "slack:@i", "slack:@j", "slack:@k", "github:" + strings.Repeat("j", 200)}},
		{Name: "Dana Smith", ImageURL: "https://secure.gravatar.com/avatar/x.png?d=mm&r=PG",
			Email: "dana@tendwell.example", Handles: []string{"pagerduty:PXPGF42"}},
	}
	var entries []Person
	for _, p := range inputs {
		if c, ok := Clean(p); ok {
			entries = append(entries, c)
		}
	}
	b, err := json.Marshal(List{Title: strings.Repeat("t", MaxTitle), Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"md/v1/leaderboard", "md/v1/oncall"} {
		if err := widgetschema.Check("environment", key, "JSON", string(b)); err != nil {
			t.Errorf("%v\n%s", err, b)
		}
	}
}

// A person that passes Validate passes the server's schema too.
func TestValidPeopleFitTheWidgetSchema(t *testing.T) {
	p := Person{Name: strings.Repeat("é", MaxName), ImageURL: "http://x.test/a", Email: "jo@x.test",
		Handles: []string{"slack:@jo", "github:jo", "a:b", "b:c", "c:d", "d:e", "e:f", "f:g", "g:h", "h:i"}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	list := List{Entries: make([]Person, MaxEntries)}
	for i := range list.Entries {
		list.Entries[i] = p
	}
	b, _ := json.Marshal(list)
	if err := widgetschema.Check("environment", "md/v1/leaderboard", "JSON", string(b)); err != nil {
		t.Error(err)
	}
}
