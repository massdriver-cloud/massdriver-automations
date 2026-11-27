package paramstogit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/itchyny/gojq"
)

// text is a value with \(expr) interpolations of jq over the event, the syntax of router's text fields. It compiles
// without an environment loader, so env and $ENV are empty and a template cannot read the GitHub token.
type text struct {
	name string
	code *gojq.Code
}

func compileText(name, src string) (*text, error) {
	lit, err := jqStringLiteral(src)
	if err != nil {
		return nil, fmt.Errorf("%s: an interpolation does not compile as jq", name)
	}
	q, err := gojq.Parse(lit)
	if err != nil {
		return nil, fmt.Errorf("%s: does not compile as jq", name)
	}
	code, err := gojq.Compile(q)
	if err != nil {
		return nil, fmt.Errorf("%s: does not compile as jq", name)
	}
	return &text{name: name, code: code}, nil
}

// eval hides the cause of a failure: jq errors quote the values they work on, which can be event data.
func (t *text) eval(ctx context.Context, event any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, ok := t.code.RunWithContext(ctx, event).Next()
	if !ok {
		return "", fmt.Errorf("%s: jq produced no output", t.name)
	}
	if _, ok := v.(error); ok {
		return "", fmt.Errorf("%s: jq evaluation failed", t.name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: output is not a string", t.name)
	}
	return s, nil
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
			return "", errors.New("no valid expression closes an interpolation")
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
