package people

import (
	"reflect"
	"strings"
	"testing"
)

func TestCleanDropsWhatTheServerRejects(t *testing.T) {
	got, ok := Clean(Person{
		Name:     "  " + strings.Repeat("é", 70) + "  ",
		ImageURL: "javascript:alert(1)",
		Email:    "jo@example.com\n",
		Handles:  []string{"github:jo", "Slack:@jo", "pagerduty:PJO"},
	})
	want := Person{Name: strings.Repeat("é", 64), Handles: []string{"github:jo", "pagerduty:PJO"}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("clean = %+v %t", got, ok)
	}
	if _, ok := Clean(Person{Name: "   ", Email: "a b@c"}); ok {
		t.Error("a person with nothing valid left is dropped")
	}
}

func TestValidate(t *testing.T) {
	ok := Person{Name: "Jo Park", ImageURL: "https://example.com/jo.png", Email: "jo@example.com",
		Handles: []string{"slack:@jo", "github:jo"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid person: %v", err)
	}
	for name, tc := range map[string]struct {
		p    Person
		want string
	}{
		"empty":       {Person{}, "set at least one of"},
		"long name":   {Person{Name: strings.Repeat("x", 65)}, "name must be"},
		"spaces":      {Person{Name: " \t"}, "name must be"},
		"two emails":  {Person{Email: "a@b.com, c@d.com"}, "email must be"},
		"newline":     {Person{Email: "a@b.com\n"}, "email must be"},
		"handle":      {Person{Handles: []string{"@jo"}}, "handles[0] must look like"},
		"no handles":  {Person{Name: "Jo", Handles: []string{}}, "handles must have 1 to 10"},
		"many":        {Person{Handles: strings.Split(strings.Repeat("a:b,", 11)[:43], ",")}, "handles must have"},
		"image space": {Person{ImageURL: "https://x/a b"}, "image_url must be"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
