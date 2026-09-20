package lint_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/render"
)

func TestGraphExport(t *testing.T) {
	cfg := config.Default()
	tree, err := render.Tree(context.Background(), filepath.Join("testdata", "deadlock"), "clusters/prod", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, edges := lint.ComponentGraph(tree, cfg)
	var cyclic, dependsOn int
	for _, e := range edges {
		if e.InCycle {
			cyclic++
		}
		if e.Kind == "dependsOn" {
			dependsOn++
		}
	}
	if cyclic != 3 || dependsOn != 1 {
		t.Errorf("want the 3 edges of the cycle marked and 1 dependsOn, got %d / %d: %+v", cyclic, dependsOn, edges)
	}

	var dot, mermaid bytes.Buffer
	lint.WriteDOT(&dot, tree, cfg)
	lint.WriteMermaid(&mermaid, tree, cfg)
	if !strings.Contains(dot.String(), `"flux-system/controllers" -> "flux-system/configs"`) || !strings.Contains(dot.String(), "color=red") {
		t.Errorf("dot:\n%s", dot.String())
	}
	if !strings.HasPrefix(mermaid.String(), "flowchart LR\n") || !strings.Contains(mermaid.String(), "stroke:red") {
		t.Errorf("mermaid:\n%s", mermaid.String())
	}

	// on an acyclic repository the critical path is what stands out
	tree, _ = render.Tree(context.Background(), filepath.Join("testdata", "timing"), "clusters/prod", cfg, nil)
	_, edges = lint.ComponentGraph(tree, cfg)
	var critical []string
	for _, e := range edges {
		if e.Critical {
			critical = append(critical, e.From+">"+e.To)
		}
	}
	if strings.Join(critical, " ") != "clusters/prod>flux-system/a flux-system/a>flux-system/b flux-system/b>flux-system/c" {
		t.Errorf("critical path root -> a -> b -> c, got %v", critical)
	}
}
