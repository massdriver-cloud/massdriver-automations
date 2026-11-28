// Package people writes lists of people to the leaderboard and on-call widgets of Massdriver environments. Both
// widgets take the same value: an optional title and 1 to 25 people, in order.
package people

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits of the widget value. The server rejects a value outside them.
const (
	MaxEntries   = 25
	MaxTitle     = 64
	MaxName      = 64
	MaxHandles   = 10
	maxURL       = 2048
	maxEmail     = 254
	maxHandleLen = 128
)

var (
	urlPattern   = regexp.MustCompile(`^https?://\S+\z`)
	emailPattern = regexp.MustCompile(`^[^@\s?&#%/,;<>"]+@[^@\s?&#%/,;<>"]+\z`)
	// HandlePattern is a service and a handle, such as slack:@jo or github:jo.
	HandlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*:\S+\z`)
	// EnvironmentPattern is an environment identifier, such as billing-prod.
	EnvironmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Person is one entry of the list. Each field is optional, but a person has at least one.
type Person struct {
	Name     string   `json:"name,omitempty" yaml:"name"`
	ImageURL string   `json:"image_url,omitempty" yaml:"image_url"`
	Email    string   `json:"email,omitempty" yaml:"email"`
	Handles  []string `json:"handles,omitempty" yaml:"handles"`
}

// List is the JSON value of a leaderboard or on-call entry.
type List struct {
	Title   string   `json:"title,omitempty"`
	Entries []Person `json:"entries"`
}

// Validate checks a person written in a config file, so a mistake fails at startup and not on each write.
func (p Person) Validate() error {
	var errs []error
	if p.Name == "" && p.ImageURL == "" && p.Email == "" && len(p.Handles) == 0 {
		errs = append(errs, errors.New("set at least one of name, image_url, email, or handles"))
	}
	if p.Name != "" && !validName(p.Name) {
		errs = append(errs, fmt.Errorf("name must be 1 to %d characters and not only spaces", MaxName))
	}
	if p.ImageURL != "" && !validURL(p.ImageURL) {
		errs = append(errs, errors.New("image_url must be an http or https URL"))
	}
	if p.Email != "" && !validEmail(p.Email) {
		errs = append(errs, errors.New("email must be one plain address, such as jo@example.com"))
	}
	if p.Handles != nil && (len(p.Handles) < 1 || len(p.Handles) > MaxHandles) {
		errs = append(errs, fmt.Errorf("handles must have 1 to %d items", MaxHandles))
	}
	for i, h := range p.Handles {
		if !validHandle(h) {
			errs = append(errs, fmt.Errorf("handles[%d] must look like service:handle, such as slack:@jo", i))
		}
	}
	return errors.Join(errs...)
}

// ValidateTitle checks an optional title.
func ValidateTitle(t string) error {
	if t != "" && utf8.RuneCountInString(t) > MaxTitle {
		return fmt.Errorf("title must be at most %d characters", MaxTitle)
	}
	return nil
}

// Clean makes a person read from another service fit the widget: it trims the name to MaxName characters and drops
// each field the server would reject. It returns false when nothing is left.
func Clean(p Person) (Person, bool) {
	var out Person
	name := strings.TrimSpace(p.Name)
	if utf8.RuneCountInString(name) > MaxName {
		name = strings.TrimSpace(string([]rune(name)[:MaxName]))
	}
	if validName(name) {
		out.Name = name
	}
	if validURL(p.ImageURL) {
		out.ImageURL = p.ImageURL
	}
	if validEmail(p.Email) {
		out.Email = p.Email
	}
	for _, h := range p.Handles {
		if validHandle(h) && len(out.Handles) < MaxHandles {
			out.Handles = append(out.Handles, h)
		}
	}
	ok := out.Name != "" || out.ImageURL != "" || out.Email != "" || len(out.Handles) > 0
	return out, ok
}

func validName(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= MaxName && strings.TrimSpace(s) != ""
}

func validURL(s string) bool { return len(s) <= maxURL && urlPattern.MatchString(s) }

func validEmail(s string) bool { return len(s) <= maxEmail && emailPattern.MatchString(s) }

func validHandle(s string) bool {
	return utf8.RuneCountInString(s) <= maxHandleLen && HandlePattern.MatchString(s)
}
