package widgetschema

import (
	"strings"
	"testing"
)

func TestPeopleWidgets(t *testing.T) {
	for _, key := range []string{"md/v1/leaderboard", "md/v1/oncall"} {
		ok := `{"title":"On call","entries":[{"name":"Jo","image_url":"https://x.test/a.png?s=1","email":"jo@x.test",` +
			`"handles":["slack:@jo","github:Jo"]}]}`
		if err := Check("environment", key, "JSON", ok); err != nil {
			t.Errorf("%s: %v", key, err)
		}
		for name, v := range map[string]string{
			"no entries":    `{"entries":[]}`,
			"empty person":  `{"entries":[{}]}`,
			"blank name":    `{"entries":[{"name":"   "}]}`,
			"long name":     `{"entries":[{"name":"` + strings.Repeat("x", 65) + `"}]}`,
			"empty title":   `{"title":"","entries":[{"name":"Jo"}]}`,
			"ftp image":     `{"entries":[{"image_url":"ftp://x.test/a.png"}]}`,
			"two emails":    `{"entries":[{"email":"a@x.test, b@x.test"}]}`,
			"bad handle":    `{"entries":[{"handles":["Slack:@jo"]}]}`,
			"no handles":    `{"entries":[{"name":"Jo","handles":[]}]}`,
			"extra field":   `{"entries":[{"name":"Jo","role":"x"}]}`,
			"26 people":     `{"entries":[` + strings.TrimSuffix(strings.Repeat(`{"name":"Jo"},`, 26), ",") + `]}`,
			"newline email": `{"entries":[{"email":"a@x.test\n"}]}`,
		} {
			if err := Check("environment", key, "JSON", v); err == nil {
				t.Errorf("%s %s: want an error", key, name)
			}
		}
	}
	if err := Check("instance", "md/v1/oncall", "JSON", `{"entries":[{"name":"Jo"}]}`); err == nil {
		t.Error("the on-call widget is on environments")
	}
}
