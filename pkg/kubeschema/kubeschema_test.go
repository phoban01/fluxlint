package kubeschema

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const doc = `{"components": {"schemas": {
  "example.Meta": {"type": "object", "properties": {"name": {"type": "string"},
    "labels": {"type": "object", "additionalProperties": {"type": "string"}}}},
  "example.IntOrString": {"oneOf": [{"type": "integer"}, {"type": "string"}]},
  "example.Tree": {"type": "object", "properties": {"value": {"type": "integer"},
    "children": {"type": "array", "items": {"$ref": "#/components/schemas/example.Tree"}}}},
  "example.Thing": {"type": "object", "required": ["spec"],
    "x-kubernetes-group-version-kind": [{"group": "things", "version": "v1", "kind": "Thing"}],
    "properties": {
      "apiVersion": {"type": "string"}, "kind": {"type": "string"},
      "metadata": {"allOf": [{"$ref": "#/components/schemas/example.Meta"}], "default": {}},
      "spec": {"type": "object", "required": ["size"], "properties": {
        "size": {"type": "integer"}, "ratio": {"type": "number"}, "enabled": {"type": "boolean"},
        "port": {"$ref": "#/components/schemas/example.IntOrString"},
        "mode": {"type": "string", "enum": ["fast", "safe"]},
        "raw": {"type": "object"},
        "tree": {"$ref": "#/components/schemas/example.Tree"},
        "replicas": {"type": "integer"}}}}}}}}`

func validate(t *testing.T, manifest string) string {
	t.Helper()
	d, err := Parse([]byte(doc), "things", "v1")
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &o); err != nil {
		t.Fatal(err)
	}
	return strings.Join(d.Validate(o), "\n")
}

func TestValid(t *testing.T) {
	got := validate(t, `apiVersion: things/v1
kind: Thing
metadata: {name: a, labels: {any-key: value}}
spec:
  size: 3
  ratio: 2
  enabled: true
  port: http
  mode: safe
  raw: {anything: [1, {goes: here}]}
  tree: {value: 1, children: [{value: 2, children: [{value: 3}]}]}
  replicas: null
`)
	if got != "" {
		t.Errorf("unexpected:\n%s", got)
	}
}

func TestInvalid(t *testing.T) {
	got := validate(t, `apiVersion: things/v1
kind: Thing
metadata: {name: a, labels: {version: 1.31}, naem: b}
spec:
  replica: 2
  ratio: "half"
  enabled: "yes"
  port: [80]
  mode: reckless
  tree: {value: 1, children: [{value: "two"}]}
`)
	for _, want := range []string{
		"metadata.labels.version: must be a string, got 1.31",
		"metadata.naem: no such field (did you mean name?)",
		"spec.replica: no such field (did you mean replicas?)",
		"spec.size: is required",
		`spec.ratio: must be a number, got the string "half"`,
		`spec.enabled: must be a boolean, got the string "yes"`,
		"spec.port: a list is not one of the forms this field accepts",
		`spec.mode: "reckless" is not one of [fast safe]`,
		`spec.tree.children[0].value: must be an integer, got the string "two"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestKinds(t *testing.T) {
	d, _ := Parse([]byte(doc), "things", "v1")
	if !d.Has("Thing") || d.Has("Other") {
		t.Error("Has")
	}
	if other, _ := Parse([]byte(doc), "things", "v2"); other.Has("Thing") {
		t.Error("a kind of another version must not be found")
	}
	if _, err := Parse([]byte(`{}`), "things", "v1"); err == nil {
		t.Error("an empty document is an error")
	}
}
