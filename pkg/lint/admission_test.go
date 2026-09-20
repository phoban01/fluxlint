package lint_test

import (
	"fmt"
	"strings"
	"testing"
)

const gadgetCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gadgets.example.test
spec:
  group: example.test
  names: {kind: Gadget, plural: gadgets}
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: false
      storage: false
      schema: {openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}}
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              required: [size, mode]
              properties:
                size: {type: integer, minimum: 1}
                mode: {type: string, enum: [fast, safe], default: safe}
`

func gadget(version, name, spec string) string {
	return fmt.Sprintf("---\napiVersion: example.test/%s\nkind: Gadget\nmetadata:\n  name: %s\n  namespace: default\nspec: %s\n", version, name, spec)
}

func TestCustomResourcesAgainstRenderedCRD(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "gadgets", "gadgets", ""))
	write(t, dir, "gadgets/crd.yaml", gadgetCRD)
	write(t, dir, "gadgets/crs.yaml",
		gadget("v1", "good", "{size: 3}")+ // mode is required but defaulted
			gadget("v1", "bad-value", "{size: 0, mode: reckless}")+
			gadget("v1", "missing", "{mode: fast}")+
			gadget("v1alpha1", "old", "{size: 1}"))
	r := analyseDir(t, dir, nil)

	schema := messages(find(r, "FL-V001"))
	for _, want := range []string{"Gadget/default/bad-value", "spec.size", "spec.mode", "Gadget/default/missing", "Required value"} {
		if !strings.Contains(schema, want) {
			t.Errorf("FL-V001 lacks %q:\n%s", want, schema)
		}
	}
	if strings.Contains(schema, "Gadget/default/good") {
		t.Errorf("defaulting must satisfy the required field:\n%s", schema)
	}
	if got := find(r, "FL-V001"); len(got) != 2 {
		t.Errorf("want 2 schema violations, got %d", len(got))
	}
	unserved := find(r, "FL-G006")
	if len(unserved) != 1 || !strings.Contains(unserved[0].Message, "serves only v1") {
		t.Errorf("v1alpha1 is not served:\n%s", messages(unserved))
	}
}

func TestPodSecurity(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "apps", "apps", ""))
	ns := func(name, level string) string {
		return fmt.Sprintf("---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels: {pod-security.kubernetes.io/enforce: %s}\n", name, level)
	}
	const hostPod = "hostNetwork: true\ncontainers:\n  - name: c\n    image: example.test/c:1\n    securityContext: {privileged: true}\n"
	const plainPod = "containers:\n  - name: c\n    image: example.test/c:1\n"
	write(t, dir, "apps/all.yaml", ns("locked", "baseline")+ns("open", "privileged")+ns("strict", "restricted")+
		deployment("agent", "locked", 1, hostPod)+
		deployment("agent", "open", 1, hostPod)+
		deployment("web", "locked", 1, plainPod)+
		deployment("web", "strict", 1, plainPod))
	r := analyseDir(t, dir, nil)
	got := find(r, "FL-V002")
	text := messages(got)
	if len(got) != 2 {
		t.Fatalf("want agent in locked (baseline) and web in strict (restricted), got:\n%s", text)
	}
	for _, want := range []string{"Deployment/locked/agent", "host namespaces", "privileged", "Deployment/strict/web", "allowPrivilegeEscalation"} {
		if !strings.Contains(text, want) {
			t.Errorf("FL-V002 lacks %q:\n%s", want, text)
		}
	}
}
