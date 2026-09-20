package lint_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/render"
	"github.com/phoban01/fluxlint/pkg/source"
)

const widgetCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.test
spec:
  group: example.test
  names: {kind: Widget, plural: widgets}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema: {openAPIV3Schema: {type: object}}
`

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// operatorRepo is an upstream repository: v1.0.0 ships the Widget CRD,
// v1.1.0 and main additionally ship a ConfigMap.
func operatorRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "config/crd/widget.yaml", widgetCRD)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "crd")
	git(t, dir, "tag", "v1.0.0")
	write(t, dir, "config/crd/extra.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: default\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "extra")
	git(t, dir, "tag", "v1.1.0")
	return dir
}

// cluster writes a repository whose `operator` Kustomization reads the
// upstream repo at ref, and whose `widgets` Kustomization applies a Widget.
func cluster(t *testing.T, upstreamURL, ref string, widgetsDependOnOperator bool) string {
	t.Helper()
	dir := t.TempDir()
	dependsOn := ""
	if widgetsDependOnOperator {
		dependsOn = "  dependsOn:\n    - name: operator\n"
	}
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: operator
  namespace: flux-system
spec:
  interval: 10m
  url: %s
  ref:
    %s
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: operator
  namespace: flux-system
spec:
  interval: 10m
  retryInterval: 1m
  prune: true
  wait: true
  timeout: 3m
  sourceRef:
    kind: GitRepository
    name: operator
  path: ./config/crd
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: widgets
  namespace: flux-system
spec:
  interval: 10m
  retryInterval: 1m
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  path: ./widgets
%s`, upstreamURL, ref, dependsOn))
	write(t, dir, "widgets/widget.yaml", "apiVersion: example.test/v1\nkind: Widget\nmetadata:\n  name: w\n  namespace: default\n")
	return dir
}

func analyseExternal(t *testing.T, repo string, opts source.Options) *lint.Result {
	t.Helper()
	res, err := source.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	tree, err := render.Tree(context.Background(), repo, "clusters/prod", cfg, res)
	if err != nil {
		t.Fatal(err)
	}
	return lint.Run(tree, cfg)
}

func TestExternalGitSourcePinnedByTag(t *testing.T) {
	upstream := operatorRepo(t)
	repo := cluster(t, "file://"+upstream, "tag: v1.0.0", true)
	cache := t.TempDir()

	r := analyseExternal(t, repo, source.Options{CacheDir: cache})
	if got := problems(r); len(got) != 0 {
		t.Fatalf("CRD comes from the rendered external repo and is ordered by dependsOn; got %v\n%+v", got, r.Findings)
	}
	op := component(t, r, "flux-system/operator")
	if len(op.Objects) != 1 || op.Objects[0].Kind() != "CustomResourceDefinition" {
		t.Fatalf("v1.0.0 has exactly the CRD, got %v", op.Objects)
	}
	if !strings.HasPrefix(op.SourceRevision, "tag v1.0.0@sha1:") {
		t.Errorf("revision = %q", op.SourceRevision)
	}
	if got := find(r, "FL-X002"); len(got) != 0 {
		t.Errorf("a tag is not a floating ref: %+v", got)
	}
	// the dependsOn is now justified by an observed import
	if got := find(r, "FL-T004"); len(got) != 0 {
		t.Errorf("widgets -> operator is justified by the CRD: %+v", got)
	}

	// A pinned tag is immutable: with the upstream gone and no network the
	// cached tree must still serve.
	if err := os.RemoveAll(upstream); err != nil {
		t.Fatal(err)
	}
	r = analyseExternal(t, repo, source.Options{CacheDir: cache, Mode: source.Offline})
	if got := problems(r); len(got) != 0 {
		t.Fatalf("offline run with warm cache: %v", got)
	}
}

func TestExternalCRDWithoutOrderingIsImplicit(t *testing.T) {
	repo := cluster(t, "file://"+operatorRepo(t), "tag: v1.0.0", false)
	r := analyseExternal(t, repo, source.Options{CacheDir: t.TempDir()})
	got := find(r, "FL-T006")
	if len(got) != 1 || !strings.Contains(got[0].Message, "example.test/Widget") {
		t.Fatalf("Widget needs the operator's CRD but nothing orders them: %+v", r.Findings)
	}
}

func TestExternalGitSourceOfflineMiss(t *testing.T) {
	repo := cluster(t, "file://"+operatorRepo(t), "tag: v1.0.0", true)
	r := analyseExternal(t, repo, source.Options{CacheDir: t.TempDir(), Mode: source.Offline})
	got := find(r, "FL-X001")
	if len(got) != 1 || !strings.Contains(got[0].Message, "offline") {
		t.Fatalf("want one source-unavailable finding, got %+v", r.Findings)
	}
	// and the CRD is then unknown rather than silently assumed
	if len(find(r, "FL-G005")) != 1 {
		t.Errorf("unknown-crd expected when the provider is not rendered: %+v", r.Findings)
	}
}

func TestExternalGitSourceFloatingRefs(t *testing.T) {
	upstream := operatorRepo(t)
	for ref, wantObjects := range map[string]int{
		"branch: main":      2, // CRD + ConfigMap
		`semver: ">=1.0.0"`: 2, // resolves to v1.1.0
		`semver: "1.0.x"`:   1, // resolves to v1.0.0
	} {
		t.Run(ref, func(t *testing.T) {
			repo := cluster(t, "file://"+upstream, ref, true)
			r := analyseExternal(t, repo, source.Options{CacheDir: t.TempDir()})
			if got := len(component(t, r, "flux-system/operator").Objects); got != wantObjects {
				t.Errorf("objects = %d, want %d", got, wantObjects)
			}
			if got := find(r, "FL-X002"); len(got) != 1 {
				t.Errorf("floating ref must be reported: %+v", r.Findings)
			}
		})
	}
}

func TestExternalGitSourceMissingTag(t *testing.T) {
	repo := cluster(t, "file://"+operatorRepo(t), "tag: v9.9.9", true)
	r := analyseExternal(t, repo, source.Options{CacheDir: t.TempDir()})
	if got := find(r, "FL-X001"); len(got) != 1 {
		t.Fatalf("a tag that does not exist must surface as source-unavailable: %+v", r.Findings)
	}
}

func TestSourceOverride(t *testing.T) {
	local := t.TempDir()
	write(t, local, "config/crd/widget.yaml", widgetCRD)
	repo := cluster(t, "https://unreachable.invalid/operator.git", "tag: v1.0.0", true)
	r := analyseExternal(t, repo, source.Options{
		CacheDir:  t.TempDir(),
		Mode:      source.Offline,
		Overrides: []source.Override{{Kind: "GitRepository", Name: "operator", Path: local}},
	})
	if got := problems(r); len(got) != 0 {
		t.Fatalf("override should satisfy the source without any network: %v\n%+v", got, r.Findings)
	}
}
