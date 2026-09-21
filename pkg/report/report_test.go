package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/model"
)

// A report that is read somewhere else must say what was looked at.
func TestJSONSaysWhatWasAnalysed(t *testing.T) {
	root := &model.Component{Name: "clusters/prod", IsRoot: true}
	ok := &model.Component{Kind: model.KindKustomization, Namespace: "flux-system", Name: "apps", Parent: root, Path: "./apps",
		Objects: []model.Object{{"kind": "ConfigMap"}}}
	blind := &model.Component{Kind: model.KindKustomization, Namespace: "flux-system", Name: "operator", Parent: root,
		External: true, Source: model.Ref{Kind: "GitRepository", Namespace: "flux-system", Name: "operator"},
		Opaque: "source GitRepository/flux-system/operator unavailable", SourceErr: errors.New("authentication required")}
	tree := &model.Tree{Entrypoint: "clusters/prod", KubeRelease: "v1.35.0", Root: root, Components: []*model.Component{root, ok, blind}}
	var buf bytes.Buffer
	if err := JSON(&buf, Summary{Version: "fluxlint test", Results: []*lint.Result{{Tree: tree}}}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version     string
		Entrypoints []struct {
			KubeRelease string
			Components  []struct {
				Component, NotRendered, Error, Source string
				Rendered                              bool
				Objects                               int
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	e := got.Entrypoints[0]
	if got.Version != "fluxlint test" || e.KubeRelease != "v1.35.0" || len(e.Components) != 3 {
		t.Fatalf("%+v", got)
	}
	if c := e.Components[1]; !c.Rendered || c.Objects != 1 || c.Component != "flux-system/apps" {
		t.Errorf("rendered component: %+v", c)
	}
	if c := e.Components[2]; c.Rendered || c.Error != "authentication required" || !strings.Contains(c.NotRendered, "unavailable") || c.Source != "GitRepository/flux-system/operator" {
		t.Errorf("a component that was not rendered must say why: %+v", c)
	}
}
