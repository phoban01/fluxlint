// Package kubeschema checks built-in objects against the OpenAPI v3 documents
// that every Kubernetes release publishes, so that "is this a valid
// Deployment" is answered for the version a cluster runs and not for the
// version of the Go types this program happens to be built with.
package kubeschema

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Doc is one published document: all kinds of one group and version.
type Doc struct {
	schemas map[string]any
	kinds   map[string]string // Kind -> schema name
}

// Parse reads api__v1_openapi.json, apis__apps__v1_openapi.json, …
func Parse(data []byte, group, version string) (*Doc, error) {
	var raw struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if len(raw.Components.Schemas) == 0 {
		return nil, fmt.Errorf("no schemas in the document")
	}
	d := &Doc{schemas: raw.Components.Schemas, kinds: map[string]string{}}
	for name, s := range d.schemas {
		m, _ := s.(map[string]any)
		gvks, _ := m["x-kubernetes-group-version-kind"].([]any)
		for _, g := range gvks {
			gvk, _ := g.(map[string]any)
			if gvk["group"] == group && gvk["version"] == version {
				d.kinds[fmt.Sprint(gvk["kind"])] = name
			}
		}
	}
	return d, nil
}

// Has reports whether the release serves kind in this group and version.
func (d *Doc) Has(kind string) bool { return d.kinds[kind] != "" }

// Validate returns what is wrong with obj as the release defines its kind:
// fields that do not exist, values of the wrong type, required fields that
// are missing. It says nothing about values (a port out of range, a selector
// that does not match): the schema does not carry those rules.
func (d *Doc) Validate(obj map[string]any) []string {
	name := d.kinds[fmt.Sprint(obj["kind"])]
	if name == "" {
		return nil
	}
	v := &validator{doc: d}
	v.value(obj, d.schemas[name], "", 0)
	sort.Strings(v.errs)
	return v.errs
}

type validator struct {
	doc  *Doc
	errs []string
}

func (v *validator) fail(path, format string, args ...any) {
	if path == "" {
		path = "(root)"
	}
	v.errs = append(v.errs, strings.TrimPrefix(path, ".")+": "+fmt.Sprintf(format, args...))
}

const refPrefix = "#/components/schemas/"

// resolve follows $ref, and the allOf-with-one-$ref that the generator wraps
// every reference in so that it can carry a description and a default.
func (v *validator) resolve(schema any) map[string]any {
	for range 8 {
		m, ok := schema.(map[string]any)
		if !ok {
			return nil
		}
		if ref, ok := m["$ref"].(string); ok {
			schema = v.doc.schemas[strings.TrimPrefix(ref, refPrefix)]
			continue
		}
		if all, ok := m["allOf"].([]any); ok && len(all) == 1 && m["properties"] == nil {
			schema = all[0]
			continue
		}
		return m
	}
	return nil
}

func typeOf(x any) string {
	switch n := x.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if n == math.Trunc(n) {
			return "integer"
		}
		return "number"
	case int, int32, int64:
		return "integer"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func (v *validator) value(x any, schema any, path string, depth int) {
	s := v.resolve(schema)
	if s == nil || x == nil || depth > 64 {
		return // no schema (any JSON), an omitted value, or a recursive type
	}
	if s["x-kubernetes-preserve-unknown-fields"] == true || s["x-kubernetes-int-or-string"] == true {
		return
	}
	for _, alt := range []string{"oneOf", "anyOf"} {
		if branches, ok := s[alt].([]any); ok && len(branches) > 0 {
			for _, b := range branches {
				probe := &validator{doc: v.doc}
				probe.value(x, b, path, depth+1)
				if len(probe.errs) == 0 {
					return
				}
			}
			v.fail(path, "%s is not one of the forms this field accepts", describe(x))
			return
		}
	}
	got := typeOf(x)
	want, _ := s["type"].(string)
	switch {
	case want == "":
	case want == got, want == "number" && got == "integer":
	default:
		v.fail(path, "must be %s, got %s", article(want), describe(x))
		return
	}
	switch val := x.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		additional, open := s["additionalProperties"]
		for _, req := range list(s["required"]) {
			if _, ok := val[fmt.Sprint(req)]; !ok {
				v.fail(path+"."+fmt.Sprint(req), "is required")
			}
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch p, known := props[k]; {
			case known:
				v.value(val[k], p, path+"."+k, depth+1)
			case open:
				v.value(val[k], additional, path+"."+k, depth+1)
			case len(props) > 0:
				v.fail(path+"."+k, "no such field%s", suggestion(k, props))
			}
		}
	case []any:
		for i, item := range val {
			v.value(item, s["items"], fmt.Sprintf("%s[%d]", path, i), depth+1)
		}
	case string:
		if enum := list(s["enum"]); len(enum) > 0 {
			ok := false
			for _, e := range enum {
				ok = ok || e == val
			}
			if !ok {
				v.fail(path, "%q is not one of %v", val, enum)
			}
		}
	}
}

func list(x any) []any { l, _ := x.([]any); return l }

func describe(x any) string {
	switch val := x.(type) {
	case string:
		return fmt.Sprintf("the string %q", val)
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	}
	return fmt.Sprintf("%v", x)
}

func article(t string) string {
	if t == "integer" || t == "object" || t == "array" {
		return "an " + t
	}
	return "a " + t
}

// suggestion names the field that was probably meant.
func suggestion(k string, props map[string]any) string {
	best, bestD := "", 3
	for p := range props {
		if d := distance(strings.ToLower(k), strings.ToLower(p)); d < bestD || (d == bestD && p < best) {
			best, bestD = p, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %s?)", best)
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
