package lint_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
)

func transitionRepo(t *testing.T, prune bool, objects string) string {
	dir := t.TempDir()
	extra := ""
	if !prune {
		extra = "  # prune disabled below\n"
	}
	ks := fmt.Sprintf(ksHeader, "apps", "apps", extra)
	if !prune {
		ks = strings.Replace(ks, "prune: true", "prune: false", 1)
	}
	write(t, dir, "clusters/prod/all.yaml", ks+"---\n"+fmt.Sprintf(ksHeader, "other", "other", ""))
	write(t, dir, "apps/all.yaml", objects)
	write(t, dir, "other/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: keep\n  namespace: default\n")
	return dir
}

const plain = "containers:\n  - name: c\n    image: example.test/c:1\n"
const pvc = "---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: data\n  namespace: default\nspec:\n  accessModes: [ReadWriteOnce]\n  resources: {requests: {storage: 1Gi}}\n"

func compare(t *testing.T, base, head string) *lint.Result {
	cfg := config.Default()
	b, h := analyseDir(t, base, cfg), analyseDir(t, head, cfg)
	lint.Compare(b, h, cfg)
	return h
}

func TestTransitionPruneAndOrphans(t *testing.T) {
	both := deployment("web", "default", 1, plain) + pvc
	r := compare(t, transitionRepo(t, true, both), transitionRepo(t, true, deployment("web", "default", 1, plain)))
	got := find(r, "FL-D001")
	if len(got) != 1 || got[0].Severity != lint.Warning || !strings.Contains(got[0].Message, "PersistentVolumeClaim/default/data") {
		t.Fatalf("deleting a PVC through prune is a warning:\n%s", messages(r.Findings))
	}

	r = compare(t, transitionRepo(t, false, both), transitionRepo(t, false, deployment("web", "default", 1, plain)))
	if got := find(r, "FL-D004"); len(got) != 1 || !strings.Contains(got[0].Message, "prune: false") {
		t.Fatalf("with prune: false the object is orphaned, not deleted:\n%s", messages(r.Findings))
	}
}

func TestTransitionImmutableSelector(t *testing.T) {
	before := deployment("web", "default", 1, plain)
	after := strings.ReplaceAll(before, "app: web", "app: website")
	r := compare(t, transitionRepo(t, true, before), transitionRepo(t, true, after))
	got := find(r, "FL-D002")
	if len(got) != 1 || !strings.Contains(strings.Join(got[0].Detail, " "), "spec.selector") {
		t.Fatalf("selector change must be reported:\n%s", messages(r.Findings))
	}
	// replicas are mutable
	r = compare(t, transitionRepo(t, true, before), transitionRepo(t, true, strings.Replace(before, "replicas: 1", "replicas: 5", 1)))
	if got := find(r, "FL-D002"); len(got) != 0 {
		t.Fatalf("scaling is not an immutable change:\n%s", messages(got))
	}
}

func TestTransitionOwnershipMove(t *testing.T) {
	base := transitionRepo(t, true, deployment("web", "default", 1, plain))
	head := transitionRepo(t, true, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: placeholder\n  namespace: default\n")
	write(t, head, "other/web.yaml", deployment("web", "default", 1, plain))
	r := compare(t, base, head)
	got := find(r, "FL-D003")
	if len(got) != 1 || !strings.Contains(got[0].Message, "moves from flux-system/apps to flux-system/other") {
		t.Fatalf("ownership move:\n%s", messages(r.Findings))
	}
	if len(find(r, "FL-D001")) != 0 {
		t.Errorf("a moved object is not a deleted object:\n%s", messages(find(r, "FL-D001")))
	}
}

func TestExistingFindingsDoNotFailABaseRun(t *testing.T) {
	broken := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: nowhere\n"
	worse := broken + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n  namespace: elsewhere\n"
	r := compare(t, transitionRepo(t, true, broken), transitionRepo(t, true, worse))
	var existing, fresh []string
	for _, f := range find(r, "FL-G004") {
		if f.Existing {
			existing = append(existing, f.Message)
		} else {
			fresh = append(fresh, f.Message)
		}
	}
	if len(existing) != 1 || !strings.Contains(existing[0], "nowhere") || len(fresh) != 1 || !strings.Contains(fresh[0], "elsewhere") {
		t.Fatalf("existing=%v fresh=%v", existing, fresh)
	}
}

const gadgetPolicies = `---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: known-mode}
spec:
  matchConstraints:
    resourceRules:
      - {apiGroups: [example.test], apiVersions: ["*"], resources: [gadgets], operations: [CREATE, UPDATE]}
  validations:
    - expression: "!has(object.metadata.labels) || !('stage' in object.metadata.labels) || object.metadata.labels['stage'] in ['dev', 'prod']"
      message: stage must be dev or prod
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: known-mode}
spec: {policyName: known-mode, validationActions: [Deny]}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: size-is-immutable}
spec:
  matchConstraints:
    resourceRules:
      - {apiGroups: ["*"], apiVersions: ["*"], resources: [gadgets], operations: [UPDATE]}
  validations:
    - expression: "!has(oldObject.spec.size) || oldObject.spec.size == object.spec.size"
      message: the size of a gadget cannot change
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: size-is-immutable}
spec: {policyName: size-is-immutable, validationActions: [Deny]}
`

func policyRepo(t *testing.T, gadgets, policies string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "gadgets", "gadgets", ""))
	write(t, dir, "gadgets/crd.yaml", gadgetCRD)
	write(t, dir, "gadgets/policies.yaml", policies)
	write(t, dir, "gadgets/crs.yaml", gadgets)
	return dir
}

func labelled(name, stage string, size int) string {
	return fmt.Sprintf("---\napiVersion: example.test/v1\nkind: Gadget\nmetadata:\n  name: %s\n  namespace: default\n  labels: {stage: %s}\nspec: {size: %d}\n", name, stage, size)
}

// The repository's own admission policies, evaluated by the API server's code.
func TestAdmissionPolicyOnCreate(t *testing.T) {
	r := analyseDir(t, policyRepo(t, labelled("ok", "prod", 1)+labelled("typo", "prd", 1), gadgetPolicies), nil)
	got := find(r, "FL-V005")
	if len(got) != 1 || got[0].Severity != lint.Error || !strings.Contains(messages(got), "Gadget/default/typo") ||
		!strings.Contains(messages(got), "known-mode") || !strings.Contains(messages(got), "stage must be dev or prod") {
		t.Errorf("want one rejection, of the typo, with the policy's message:\n%s", messages(got))
	}
}

// oldObject rules can only speak when there is an old object: with --base.
func TestAdmissionPolicyOnUpdate(t *testing.T) {
	base := policyRepo(t, labelled("a", "prod", 1)+labelled("b", "prod", 1), gadgetPolicies)
	head := policyRepo(t, labelled("a", "prod", 2)+labelled("b", "dev", 1), gadgetPolicies)
	got := find(compare(t, base, head), "FL-V005")
	if len(got) != 1 || !strings.Contains(messages(got), "Gadget/default/a") || !strings.Contains(messages(got), "rejects this update: the size of a gadget cannot change") {
		t.Errorf("want the resize of a rejected, and the relabel of b allowed:\n%s", messages(got))
	}
	if alone := find(analyseDir(t, head, nil), "FL-V005"); len(alone) != 0 {
		t.Errorf("without a base nothing is an update:\n%s", messages(alone))
	}
}

func TestAdmissionPolicyThatDoesNotCompile(t *testing.T) {
	broken := strings.Replace(gadgetPolicies, "oldObject.spec.size == object.spec.size", "oldObject.spec.size = object.spec.size", 1)
	got := find(analyseDir(t, policyRepo(t, labelled("a", "prod", 1), broken), nil), "FL-V006")
	if len(got) != 1 || !strings.Contains(messages(got), "ValidatingAdmissionPolicy/size-is-immutable") {
		t.Errorf("got:\n%s", messages(got))
	}
}
