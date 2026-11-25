package router

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/itchyny/gojq"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

const (
	engineJQ = "jq"
	engineGo = "go"

	exprTimeout = 2 * time.Second
)

// Routes run code from the configuration, so they must not see the credentials of the router itself.
var hiddenEnv = []string{"MASSDRIVER_SIGNING_SECRET", "MASSDRIVER_API_KEY"}

func routeEnviron() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !isHidden(name) {
			env = append(env, kv)
		}
	}
	return env
}

func isHidden(name string) bool {
	for _, h := range hiddenEnv {
		if name == h {
			return true
		}
	}
	return false
}

type compiler struct {
	engine string
}

func newCompiler(engine string) *compiler { return &compiler{engine: engine} }

// program is one compiled expression: a jq program, or a Go template.
type program struct {
	name string
	jq   *gojq.Code
	tmpl *template.Template
}

// input is what every expression sees: the event as Massdriver sent it.
type input struct {
	ev   automation.Event
	data any
}

// text compiles a value that is mostly literal, such as a URL or a command argument. Under jq, each `\(expr)`
// interpolates and the rest stays literal, so quotes and backslashes in a shell argument need no escaping.
func (c *compiler) text(name, src string) (*program, error) {
	if c.engine != engineJQ {
		return c.tmpl(name, src)
	}
	lit, err := jqStringLiteral(src)
	if err != nil {
		return nil, fmt.Errorf("%s: jq: %w", name, err)
	}
	return c.jq(name, lit)
}

// jqStringLiteral turns text with `\(expr)` interpolations into a jq string literal. An interpolation ends at the
// first `)` that closes a valid jq expression, as jq itself reads it.
func jqStringLiteral(src string) (string, error) {
	var b strings.Builder
	b.WriteByte('"')
	for {
		start := strings.Index(src, `\(`)
		if start < 0 {
			b.WriteString(jqEscape(src))
			break
		}
		b.WriteString(jqEscape(src[:start]))
		rest := src[start+2:]
		end := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] != ')' {
				continue
			}
			if _, err := gojq.Parse(rest[:i]); err == nil {
				end = i
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("no valid expression closes %q", src[start:])
		}
		b.WriteString(`\(` + rest[:end] + `)`)
		src = rest[end+1:]
	}
	b.WriteByte('"')
	return b.String(), nil
}

func jqEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func (c *compiler) value(name, src string) (*program, error) {
	if c.engine == engineJQ {
		return c.jq(name, src)
	}
	return c.tmpl(name, src)
}

func (c *compiler) jq(name, src string) (*program, error) {
	q, err := gojq.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("%s: jq: %w", name, err)
	}
	code, err := gojq.Compile(q, gojq.WithEnvironLoader(routeEnviron))
	if err != nil {
		return nil, fmt.Errorf("%s: jq: %w", name, err)
	}
	return &program{name: name, jq: code}, nil
}

func (c *compiler) tmpl(name, src string) (*program, error) {
	t, err := template.New(name).Option("missingkey=error").Funcs(templateFuncs(automation.Event{})).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("%s: template: %w", name, err)
	}
	return &program{name: name, tmpl: t}, nil
}

// eval returns the first output of a jq program, or the text of a template.
func (p *program) eval(ctx context.Context, in input) (any, error) {
	if p.jq != nil {
		ctx, cancel := context.WithTimeout(ctx, exprTimeout)
		defer cancel()
		v, ok := p.jq.RunWithContext(ctx, in.data).Next()
		if !ok {
			return nil, fmt.Errorf("%s: jq produced no output", p.name)
		}
		if err, ok := v.(error); ok {
			return nil, fmt.Errorf("%s: jq: %w", p.name, err)
		}
		return v, nil
	}
	t, err := p.tmpl.Clone()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := t.Funcs(templateFuncs(in.ev)).Execute(&b, in.data); err != nil {
		return nil, fmt.Errorf("%s: template: %w", p.name, err)
	}
	return b.String(), nil
}

func (p *program) text(ctx context.Context, in input) (string, error) {
	v, err := p.eval(ctx, in)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: output is %s, not a string", p.name, jsonType(v))
	}
	return s, nil
}

// truth: jq must output true, and a template must render "true".
func (p *program) truth(ctx context.Context, in input) (bool, error) {
	v, err := p.eval(ctx, in)
	if err != nil {
		return false, err
	}
	if p.tmpl != nil {
		return strings.TrimSpace(v.(string)) == "true", nil
	}
	return v == true, nil
}

// body: jq output is encoded as JSON, and template output is sent as it is.
func (p *program) body(ctx context.Context, in input) (string, error) {
	v, err := p.eval(ctx, in)
	if err != nil {
		return "", err
	}
	if p.tmpl != nil {
		return v.(string), nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// object: jq must output an object, and a template must render a JSON object.
func (p *program) object(ctx context.Context, in input) (map[string]any, error) {
	v, err := p.eval(ctx, in)
	if err != nil {
		return nil, err
	}
	if s, ok := v.(string); ok && p.tmpl != nil {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, fmt.Errorf("%s: template output is not a JSON object: %w", p.name, err)
		}
		return m, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: output is %s, not an object", p.name, jsonType(v))
	}
	return m, nil
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return "a number"
	}
}

func templateFuncs(ev automation.Event) template.FuncMap {
	return template.FuncMap{
		"attr": func(key string) (string, error) {
			if _, ok := ev.Data.Attributes[key]; !ok {
				return "", fmt.Errorf("event has no attribute %q", key)
			}
			return ev.Attribute(key), nil
		},
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
		"env": func(name string) string {
			if isHidden(name) {
				return ""
			}
			return os.Getenv(name)
		},
	}
}
