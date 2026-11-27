package paramstogit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

const (
	masked = "[SENSITIVE]"
	// The deployment queue adds md_metadata, which changes on every deployment and is not configuration.
	metadataKey = "md_metadata"
	maxDepth    = 64
)

// render writes params as stable JSON: sorted keys, two-space indent, and a trailing newline, so the same params
// always produce the same bytes and an unchanged deployment produces no commit.
func render(raw json.RawMessage, schema map[string]any, r *redactor) ([]byte, error) {
	params := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		// Numbers keep their text: a float64 would turn a large integer into 1e+21 and change the file.
		dec.UseNumber()
		if err := dec.Decode(&params); err != nil {
			return nil, err
		}
	}
	delete(params, metadataKey)
	r.object(params, []map[string]any{schema}, "", schema)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(params); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// redactor masks or omits the params that the schema marks sensitive.
type redactor struct {
	omit bool
	// key turns a mask into a keyed fingerprint, so a diff shows that a secret changed. Without it, every mask is
	// the same text.
	key   []byte
	scope string
}

// object walks obj with every subschema that can describe it. Bundle schemas put fields in dependencies, oneOf,
// and allOf branches, and a field marked sensitive in any branch must never be written.
func (r *redactor) object(obj map[string]any, views []map[string]any, ptr string, root map[string]any) {
	all := expand(views, root)
	for key, val := range obj {
		props := propertySchemas(all, key)
		p := ptr + "/" + escapePointer(key)
		if sensitive(expand(props, root)) {
			if r.omit {
				delete(obj, key)
			} else {
				obj[key] = r.mask(p, val)
			}
			continue
		}
		obj[key] = r.value(val, props, p, root)
	}
}

func (r *redactor) value(val any, views []map[string]any, ptr string, root map[string]any) any {
	switch v := val.(type) {
	case map[string]any:
		r.object(v, views, ptr, root)
	case []any:
		items := itemSchemas(expand(views, root))
		out := v[:0]
		for i, item := range v {
			p := ptr + "/" + strconv.Itoa(i)
			if sensitive(expand(items, root)) {
				if r.omit {
					continue
				}
				out = append(out, r.mask(p, item))
				continue
			}
			out = append(out, r.value(item, items, p, root))
		}
		return out
	}
	return val
}

func (r *redactor) mask(ptr string, val any) string {
	if len(r.key) == 0 {
		return masked
	}
	b, _ := json.Marshal(val)
	mac := hmac.New(sha256.New, r.key)
	// The instance and the path are in the input, so one secret reused in two places gets two fingerprints.
	mac.Write([]byte(r.scope + "\x00" + ptr + "\x00"))
	mac.Write(b)
	return "[SENSITIVE:" + hex.EncodeToString(mac.Sum(nil))[:16] + "]"
}

// Massdriver masks $md.sensitive. format: password and writeOnly are how bundle authors mark secret params in the
// form, so they count too.
func sensitive(views []map[string]any) bool {
	for _, s := range views {
		if s["$md.sensitive"] == true || s["writeOnly"] == true || s["format"] == "password" {
			return true
		}
	}
	return false
}

// expand adds the subschemas that apply to the same value: $ref targets and the branches of allOf, anyOf, oneOf,
// if/then/else, and dependencies.
func expand(views []map[string]any, root map[string]any) []map[string]any {
	var out []map[string]any
	var add func(s map[string]any, depth int)
	add = func(s map[string]any, depth int) {
		if s == nil || depth > maxDepth {
			return
		}
		out = append(out, s)
		if ref, ok := s["$ref"].(string); ok {
			add(resolveRef(ref, root), depth+1)
		}
		for _, k := range []string{"allOf", "anyOf", "oneOf"} {
			if list, ok := s[k].([]any); ok {
				for _, item := range list {
					sub, _ := item.(map[string]any)
					add(sub, depth+1)
				}
			}
		}
		for _, k := range []string{"if", "then", "else"} {
			sub, _ := s[k].(map[string]any)
			add(sub, depth+1)
		}
		if deps, ok := s["dependencies"].(map[string]any); ok {
			for _, d := range deps {
				sub, _ := d.(map[string]any)
				add(sub, depth+1)
			}
		}
	}
	for _, v := range views {
		add(v, 0)
	}
	return out
}

func propertySchemas(views []map[string]any, key string) []map[string]any {
	var out []map[string]any
	for _, s := range views {
		props, _ := s["properties"].(map[string]any)
		if p, ok := props[key].(map[string]any); ok {
			out = append(out, p)
			continue
		}
		if ap, ok := s["additionalProperties"].(map[string]any); ok {
			out = append(out, ap)
		}
	}
	return out
}

func itemSchemas(views []map[string]any) []map[string]any {
	var out []map[string]any
	for _, s := range views {
		switch items := s["items"].(type) {
		case map[string]any:
			out = append(out, items)
		case []any:
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
		if prefix, ok := s["prefixItems"].([]any); ok {
			for _, it := range prefix {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// resolveRef follows a local reference, such as #/definitions/password or #/$defs/password.
func resolveRef(ref string, root map[string]any) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = root
	for _, part := range strings.Split(ref[2:], "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
	}
	out, _ := cur.(map[string]any)
	return out
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
