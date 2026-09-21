package policy

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func objects(t *testing.T, docs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, doc := range strings.Split(docs, "\n---\n") {
		var o map[string]any
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatal(err)
		}
		if o != nil {
			out = append(out, o)
		}
	}
	return out
}

const policies = `apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: known-tier}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - {apiGroups: [example.test], apiVersions: [v1], resources: [widgets], operations: [CREATE, UPDATE]}
  variables:
    - {name: tier, expression: "has(object.metadata.labels) && 'tier' in object.metadata.labels ? object.metadata.labels['tier'] : ''"}
  validations:
    - expression: "variables.tier == '' || variables.tier in ['gold', 'silver']"
      messageExpression: "'tier ' + variables.tier + ' is not one of gold, silver'"
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: known-tier}
spec: {policyName: known-tier, validationActions: [Deny]}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: serial-is-immutable}
spec:
  matchConstraints:
    resourceRules:
      - {apiGroups: ["*"], apiVersions: ["*"], resources: [widgets], operations: [UPDATE]}
  validations:
    - expression: "!has(oldObject.spec.serial) || oldObject.spec.serial == object.spec.serial"
      message: the serial of a widget cannot change
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: serial-is-immutable}
spec:
  policyName: serial-is-immutable
  validationActions: [Deny]
  matchResources:
    namespaceSelector: {matchLabels: {guarded: "true"}}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: only-audited}
spec:
  matchConstraints:
    resourceRules:
      - {apiGroups: [example.test], apiVersions: [v1], resources: [widgets], operations: [CREATE]}
  validations: [{expression: "false"}]
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: only-audited}
spec: {policyName: only-audited, validationActions: [Audit]}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: capped}
spec:
  paramKind: {apiVersion: v1, kind: ConfigMap}
  matchConstraints:
    resourceRules:
      - {apiGroups: [example.test], apiVersions: [v1], resources: [widgets], operations: [CREATE]}
  validations:
    - expression: "object.spec.size <= int(params.data.max)"
      messageExpression: "'size is over ' + params.data.max"
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: capped}
spec:
  policyName: capped
  validationActions: [Warn]
  paramRef: {name: widget-limits, namespace: guarded-ns, parameterNotFoundAction: Deny}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: widget-limits, namespace: guarded-ns}
data: {max: "5"}
---
apiVersion: v1
kind: Namespace
metadata: {name: guarded-ns, labels: {guarded: "true"}}
---
apiVersion: v1
kind: Namespace
metadata: {name: open-ns}
`

func widget(namespace, tier string, serial string, size int) map[string]any {
	meta := map[string]any{"name": "w", "namespace": namespace}
	if tier != "" {
		meta["labels"] = map[string]any{"tier": tier}
	}
	return map[string]any{"apiVersion": "example.test/v1", "kind": "Widget", "metadata": meta,
		"spec": map[string]any{"serial": serial, "size": int64(size)}}
}

func TestValidatingAdmissionPolicies(t *testing.T) {
	s := NewSet(objects(t, policies))
	if len(s.CompileErrors) > 0 {
		t.Fatal(s.CompileErrors)
	}
	const flux = "system:serviceaccount:flux-system:kustomize-controller"
	eval := func(obj, old map[string]any) string {
		var out []string
		for _, v := range s.Evaluate(context.Background(), Request{Object: obj, Old: old, Resource: "widgets", User: flux}) {
			line := v.Policy + " " + v.Action + ": " + v.Message
			if v.Undecided {
				line = v.Policy + " undecided: " + v.Message
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}

	if got := eval(widget("guarded-ns", "gold", "a", 1), nil); got != "" {
		t.Errorf("a valid widget: %s", got)
	}
	if got := eval(widget("guarded-ns", "bronze", "a", 1), nil); got != "known-tier Deny: tier bronze is not one of gold, silver" {
		t.Errorf("create with an unknown tier: %q", got)
	}
	if got := eval(widget("guarded-ns", "", "a", 9), nil); got != "capped Warn: size is over 5" {
		t.Errorf("a parameter from Git: %q", got)
	}
	// the immutability policy only speaks on update, and only where bound
	if got := eval(widget("guarded-ns", "", "b", 1), widget("guarded-ns", "", "a", 1)); got != "serial-is-immutable Deny: the serial of a widget cannot change" {
		t.Errorf("update of an immutable field: %q", got)
	}
	if got := eval(widget("open-ns", "", "b", 1), widget("open-ns", "", "a", 1)); got != "" {
		t.Errorf("the binding does not select open-ns: %q", got)
	}
	// a namespace that is not in Git cannot be matched against a selector
	if got := eval(widget("elsewhere", "", "b", 1), widget("elsewhere", "", "a", 1)); got != "" {
		t.Errorf("unknown namespace: %q", got)
	}
}

func TestPolicyThatDoesNotCompile(t *testing.T) {
	s := NewSet(objects(t, strings.Replace(policies, "variables.tier == ''", "variables.tier = ''", 1)))
	if err := s.CompileErrors["known-tier"]; err == nil {
		t.Fatal("want a compile error for known-tier")
	}
}

func TestParameterNotInGit(t *testing.T) {
	s := NewSet(objects(t, strings.Replace(policies, "name: widget-limits, namespace: guarded-ns}\ndata", "name: other, namespace: guarded-ns}\ndata", 1)))
	got := s.Evaluate(context.Background(), Request{Object: widget("guarded-ns", "", "a", 9), Resource: "widgets", User: "x"})
	if len(got) != 1 || !got[0].Undecided || !strings.Contains(got[0].Message, "not rendered from Git") {
		t.Errorf("%+v", got)
	}
}
