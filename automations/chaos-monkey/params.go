package chaosmonkey

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
)

type schema = map[string]any

// preset is one entry of the params schema's `examples`, which the Massdriver UI offers as presets.
type preset struct {
	Name   string
	Params map[string]any
}

func presets(s schema) []preset {
	list, _ := s["examples"].([]any)
	var out []preset
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		p := clone(m).(map[string]any)
		name, _ := p["__name"].(string)
		delete(p, "__name")
		if name == "" {
			name = fmt.Sprintf("preset %d", i+1)
		}
		out = append(out, preset{Name: name, Params: p})
	}
	return out
}

// mess is the params a strategy made, and what it did, for the deployment message and Slack.
type mess struct {
	Strategy string
	Detail   string
	Params   map[string]any
}

type monkey struct {
	r   *rand.Rand
	cfg *Config
}

// applicable lists the strategies that can run against s, with their weights.
func (m *monkey) applicable(s schema) map[string]int {
	out := map[string]int{}
	for k, w := range m.cfg.Strategies {
		if w > 0 && fits(k, s) {
			out[k] = w
		}
	}
	return out
}

// fits reports whether a strategy can run against s: preset needs a preset, and merge needs two.
func fits(strategy string, s schema) bool {
	switch strategy {
	case StrategyPreset:
		return len(presets(s)) > 0
	case StrategyMerge:
		return len(presets(s)) > 1
	}
	return true
}

func (m *monkey) pick(weights map[string]int) string {
	keys := make([]string, 0, len(weights))
	total := 0
	for k, w := range weights {
		keys = append(keys, k)
		total += w
	}
	if total == 0 {
		return ""
	}
	sort.Strings(keys)
	n := m.r.IntN(total)
	for _, k := range keys {
		if n < weights[k] {
			return k
		}
		n -= weights[k]
	}
	return keys[len(keys)-1]
}

// Make picks a strategy that fits the schema and makes params from the instance's current params. It returns false
// when no configured strategy fits.
func (m *monkey) Make(s schema, current map[string]any) (mess, bool) {
	strategy := m.pick(m.applicable(s))
	if strategy == "" {
		return mess{}, false
	}
	return m.run(strategy, s, current), true
}

func (m *monkey) run(strategy string, s schema, current map[string]any) mess {
	base, _ := clone(current).(map[string]any)
	if base == nil {
		base = map[string]any{}
	}
	switch strategy {
	case StrategyPreset:
		ps := presets(s)
		p := ps[m.r.IntN(len(ps))]
		return mess{Strategy: strategy, Detail: fmt.Sprintf("preset %q", p.Name), Params: deepMerge(base, p.Params)}
	case StrategyMerge:
		ps := presets(s)
		m.r.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
		ps = ps[:2+m.r.IntN(len(ps)-1)]
		names := make([]string, len(ps))
		for i, p := range ps {
			base = deepMerge(base, p.Params)
			names[i] = fmt.Sprintf("%q", p.Name)
		}
		return mess{Strategy: strategy, Detail: "presets " + strings.Join(names, " + "), Params: base}
	case StrategyMutate:
		var changed []string
		out := m.mutateObject(s, base, "", &changed)
		if len(changed) == 0 {
			// A low rate can change nothing. Change one field so the run does something.
			if leaves := writable(leafPaths(s, "")); len(leaves) > 0 {
				l := leaves[m.r.IntN(len(leaves))]
				setPath(out, l.path, m.valueFor(l.schema, getPath(out, l.path)))
				changed = append(changed, l.path)
			}
		}
		return mess{Strategy: strategy, Detail: "changed " + summarize(changed), Params: out}
	default:
		leaves := leafPaths(s, "")
		n := 1 + m.r.IntN(m.cfg.Gibberish.MaxFields)
		var touched []string
		if len(leaves) == 0 {
			k := "chaos_" + m.word(6)
			base[k] = m.junk()
			touched = append(touched, k)
		}
		for i := 0; i < n && len(leaves) > 0; i++ {
			j := m.r.IntN(len(leaves))
			setPath(base, leaves[j].path, m.junk())
			touched = append(touched, leaves[j].path)
			leaves = append(leaves[:j], leaves[j+1:]...)
		}
		return mess{Strategy: StrategyGibberish, Detail: "garbled " + summarize(touched), Params: base}
	}
}

func summarize(paths []string) string {
	if len(paths) == 0 {
		return "nothing"
	}
	sort.Strings(paths)
	if len(paths) > 5 {
		return strings.Join(paths[:5], ", ") + fmt.Sprintf(" and %d more", len(paths)-5)
	}
	return strings.Join(paths, ", ")
}

// mutateObject changes each writable field of an object schema with probability mutate.rate.
func (m *monkey) mutateObject(s schema, v map[string]any, prefix string, changed *[]string) map[string]any {
	props, _ := s["properties"].(map[string]any)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ps, ok := props[k].(map[string]any)
		if !ok || readOnly(ps) {
			continue
		}
		path := join(prefix, k)
		if typeOf(ps) == "object" && ps["properties"] != nil {
			child, _ := v[k].(map[string]any)
			if child == nil {
				child = map[string]any{}
			}
			v[k] = m.mutateObject(ps, child, path, changed)
			continue
		}
		if m.r.Float64() < m.cfg.Mutate.Rate {
			v[k] = m.valueFor(ps, v[k])
			*changed = append(*changed, path)
		}
	}
	return v
}

// valueFor returns a new value for a field, mostly within its schema. Patterns and formats are not followed, so
// some values fail validation.
func (m *monkey) valueFor(s schema, current any) any {
	if c, ok := s["const"]; ok {
		return c
	}
	if opts := choices(s); len(opts) > 0 {
		return opts[m.r.IntN(len(opts))]
	}
	if ex, ok := s["examples"].([]any); ok && len(ex) > 0 && m.r.IntN(2) == 0 {
		return clone(ex[m.r.IntN(len(ex))])
	}
	switch typeOf(s) {
	case "boolean":
		if b, ok := current.(bool); ok {
			return !b
		}
		return m.r.IntN(2) == 0
	case "integer":
		lo, hi := bounds(s, current)
		ilo, ihi := int64(math.Ceil(lo)), int64(math.Floor(hi))
		if ihi < ilo {
			ihi = ilo
		}
		return ilo + m.r.Int64N(ihi-ilo+1)
	case "number":
		lo, hi := bounds(s, current)
		return math.Round((lo+m.r.Float64()*(hi-lo))*100) / 100
	case "string":
		lo, hi := 3, 16
		if v, ok := num(s["minLength"]); ok {
			lo = int(v)
		}
		if v, ok := num(s["maxLength"]); ok {
			hi = int(v)
		}
		if hi < lo {
			hi = lo
		}
		return m.word(lo + m.r.IntN(hi-lo+1))
	case "array":
		list, _ := clone(current).([]any)
		item, _ := s["items"].(map[string]any)
		switch {
		case len(list) > 0 && m.r.IntN(3) == 0:
			i := m.r.IntN(len(list))
			return append(list[:i], list[i+1:]...)
		case len(list) > 1 && m.r.IntN(2) == 0:
			m.r.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
			return list
		default:
			var proto any
			if len(list) > 0 {
				proto = list[m.r.IntN(len(list))]
			}
			return append(list, m.valueFor(item, proto))
		}
	case "object":
		out, _ := clone(current).(map[string]any)
		if out == nil {
			out = map[string]any{}
		}
		var ignored []string
		return m.mutateObject(s, out, "", &ignored)
	}
	return m.junk()
}

// junk is a value that no sensible schema accepts in the field it lands in.
func (m *monkey) junk() any {
	switch m.r.IntN(12) {
	case 0:
		return nil
	case 1:
		return -1
	case 2:
		return math.MaxInt32 + m.r.IntN(1000)
	case 3:
		return 0.000001
	case 4:
		return ""
	case 5:
		return strings.Repeat(m.word(8), 512)
	case 6:
		return "'; DROP TABLE instances; --"
	case 7:
		return "../../../../etc/passwd"
	case 8:
		return "ʇɐɥʍ ¿ zalgo ̴̛̮t̷͎̓e̵̛͜x̶̜͝t̸̰̾"
	case 9:
		return map[string]any{"chaos": map[string]any{"monkey": []any{true, nil, "banana"}}}
	case 10:
		return []any{m.word(4), 42, false}
	default:
		return m.r.IntN(2) == 0
	}
}

const letters = "abcdefghijklmnopqrstuvwxyz0123456789-"

func (m *monkey) word(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[m.r.IntN(len(letters))]
	}
	return string(b)
}

type leaf struct {
	path   string
	schema schema
}

// leafPaths lists the non-object fields of an object schema, as dotted paths.
func leafPaths(s schema, prefix string) []leaf {
	props, _ := s["properties"].(map[string]any)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []leaf
	for _, k := range keys {
		ps, ok := props[k].(map[string]any)
		if !ok {
			continue
		}
		path := join(prefix, k)
		if typeOf(ps) == "object" && ps["properties"] != nil {
			out = append(out, leafPaths(ps, path)...)
			continue
		}
		out = append(out, leaf{path: path, schema: ps})
	}
	return out
}

func writable(leaves []leaf) []leaf {
	out := leaves[:0:0]
	for _, l := range leaves {
		if !readOnly(l.schema) {
			out = append(out, l)
		}
	}
	return out
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

func getPath(v map[string]any, path string) any {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := v[p].(map[string]any)
		if !ok {
			return nil
		}
		v = next
	}
	return v[parts[len(parts)-1]]
}

func setPath(v map[string]any, path string, val any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := v[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			v[p] = next
		}
		v = next
	}
	v[parts[len(parts)-1]] = val
}

func readOnly(s schema) bool {
	b, _ := s["readOnly"].(bool)
	return b
}

// typeOf is the first non-null type of a schema.
func typeOf(s schema) string {
	switch t := s["type"].(type) {
	case string:
		return t
	case []any:
		for _, x := range t {
			if str, ok := x.(string); ok && str != "null" {
				return str
			}
		}
	}
	if _, ok := s["properties"]; ok {
		return "object"
	}
	return ""
}

// choices are the values an enum, or a oneOf or anyOf of consts, allows.
func choices(s schema) []any {
	if e, ok := s["enum"].([]any); ok {
		return e
	}
	var out []any
	for _, key := range []string{"oneOf", "anyOf"} {
		list, _ := s[key].([]any)
		for _, o := range list {
			om, _ := o.(map[string]any)
			if c, ok := om["const"]; ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func bounds(s schema, current any) (float64, float64) {
	lo, hasLo := num(s["minimum"])
	hi, hasHi := num(s["maximum"])
	if v, ok := num(s["exclusiveMinimum"]); ok {
		lo, hasLo = v+1, true
	}
	if v, ok := num(s["exclusiveMaximum"]); ok {
		hi, hasHi = v-1, true
	}
	c, _ := num(current)
	spread := math.Max(10, math.Abs(c))
	switch {
	case hasLo && hasHi:
	case hasLo:
		hi = math.Max(lo, c) + spread
	case hasHi:
		lo = math.Min(hi, c) - spread
	default:
		lo, hi = c-spread, c+spread
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// deepMerge merges src into dst. Objects merge key by key; anything else in src replaces what dst has.
func deepMerge(dst, src map[string]any) map[string]any {
	for k, sv := range src {
		sm, sok := sv.(map[string]any)
		dm, dok := dst[k].(map[string]any)
		if sok && dok {
			dst[k] = deepMerge(dm, sm)
			continue
		}
		dst[k] = clone(sv)
	}
	return dst
}

func clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = clone(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = clone(x)
		}
		return out
	}
	return v
}
