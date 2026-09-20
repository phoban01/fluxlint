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
