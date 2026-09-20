package lint

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/graph"
	"github.com/phoban01/fluxlint/pkg/model"
)

// GraphEdge is one relation between two components, as drawn by `fluxlint graph`.
type GraphEdge struct {
	From, To string
	Kind     string // dependsOn, parent, wait, need
	Label    string
	Critical bool
	InCycle  bool
}

// ComponentGraph collapses the start/ready event graph to one node per
// component, which is the picture people have in their heads.
func ComponentGraph(t *model.Tree, cfg *config.Config) (nodes []*model.Component, edges []GraphEdge) {
	ix := BuildIndex(t, cfg)
	critical := map[string]bool{}
	if s, ok := declaredGraph(ix).Solve(); ok {
		for _, e := range s.CriticalPath() {
			critical[e.From+">"+e.To] = true
		}
	}
	cyclic := map[string]bool{}
	full := fullGraph(ix)
	for _, scc := range full.Cycles() {
		for _, e := range full.EdgesWithin(scc) {
			cyclic[e.From+">"+e.To] = true
		}
	}
	mark := func(e graph.Edge, ge GraphEdge) GraphEdge {
		ge.Critical, ge.InCycle = critical[e.From+">"+e.To], cyclic[e.From+">"+e.To]
		return ge
	}
	for _, c := range t.Components {
		if p := c.Parent; p != nil {
			kind := "parent"
			e := graph.Edge{From: startOf(p), To: startOf(c)}
			ge := mark(e, GraphEdge{From: p.String(), To: c.String(), Kind: kind})
			if p.Wait {
				w := mark(graph.Edge{From: readyOf(c), To: readyOf(p)}, GraphEdge{})
				ge.Kind, ge.Critical, ge.InCycle = "parent+wait", ge.Critical || w.Critical, ge.InCycle || w.InCycle
			}
			edges = append(edges, ge)
		}
		for _, d := range c.DependsOn {
			if dep := t.ByKey[c.DepKey(d)]; dep != nil {
				e := graph.Edge{From: readyOf(dep), To: startOf(c)}
				edges = append(edges, mark(e, GraphEdge{From: dep.String(), To: c.String(), Kind: "dependsOn"}))
			}
		}
	}
	seen := map[string]bool{}
	for _, n := range ix.Needs {
		if !n.matters() {
			continue
		}
		key := n.Provider.String() + ">" + n.Consumer.String() + ">" + string(n.Kind)
		if seen[key] {
			continue
		}
		seen[key] = true
		edges = append(edges, mark(needEdge(n), GraphEdge{From: n.Provider.String(), To: n.Consumer.String(), Kind: "need", Label: string(n.Kind) + " " + n.What}))
	}
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].From+edges[i].To < edges[j].From+edges[j].To })
	return t.Components, edges
}

func nodeLabel(c *model.Component) string {
	label := c.String()
	if b := healthBound(c); b > 0 {
		label += fmt.Sprintf("\\n≤ %v", b)
	}
	if c.Opaque != "" {
		label += "\\n(not rendered)"
	}
	return label
}

// WriteDOT renders the component graph for Graphviz.
func WriteDOT(w io.Writer, t *model.Tree, cfg *config.Config) {
	nodes, edges := ComponentGraph(t, cfg)
	fmt.Fprintf(w, "digraph %q {\n  rankdir=LR;\n  node [shape=box, fontname=\"Helvetica\", fontsize=10];\n  edge [fontname=\"Helvetica\", fontsize=9];\n", t.Entrypoint)
	for _, c := range nodes {
		attrs := []string{fmt.Sprintf("label=\"%s\"", nodeLabel(c))}
		if c.IsHelmRelease() {
			attrs = append(attrs, "style=rounded")
		}
		if c.Opaque != "" {
			attrs = append(attrs, "color=gray", "fontcolor=gray")
		}
		fmt.Fprintf(w, "  %q [%s];\n", c.String(), strings.Join(attrs, ", "))
	}
	for _, e := range edges {
		var attrs []string
		switch e.Kind {
		case "parent":
			attrs = append(attrs, "style=dotted", "arrowhead=open")
		case "parent+wait":
			attrs = append(attrs, "style=dotted", "arrowhead=open", "label=\"wait\"")
		case "need":
			attrs = append(attrs, "style=dashed", fmt.Sprintf("label=%q", e.Label))
		}
		if e.Critical {
			attrs = append(attrs, "penwidth=2.5")
		}
		if e.InCycle {
			attrs = append(attrs, "color=red", "fontcolor=red")
		}
		fmt.Fprintf(w, "  %q -> %q [%s];\n", e.From, e.To, strings.Join(attrs, ", "))
	}
	fmt.Fprintln(w, "}")
}

// WriteMermaid renders the component graph as a Mermaid flowchart, which
// GitLab and GitHub display inline in Markdown.
func WriteMermaid(w io.Writer, t *model.Tree, cfg *config.Config) {
	nodes, edges := ComponentGraph(t, cfg)
	id := map[string]string{}
	fmt.Fprintln(w, "flowchart LR")
	for i, c := range nodes {
		id[c.String()] = fmt.Sprintf("n%d", i)
		label := strings.ReplaceAll(nodeLabel(c), "\\n", "<br/>")
		open, shut := "[", "]"
		if c.IsHelmRelease() {
			open, shut = "(", ")"
		}
		fmt.Fprintf(w, "  %s%s\"%s\"%s\n", id[c.String()], open, label, shut)
	}
	var critical, cyclic []string
	for i, e := range edges {
		arrow := "-->"
		switch e.Kind {
		case "parent":
			arrow = "-.->"
		case "parent+wait":
			arrow = "-. wait .->"
		case "need":
			arrow = fmt.Sprintf("-. \"%s\" .->", strings.ReplaceAll(e.Label, "\"", "'"))
		}
		fmt.Fprintf(w, "  %s %s %s\n", id[e.From], arrow, id[e.To])
		if e.InCycle {
			cyclic = append(cyclic, fmt.Sprint(i))
		} else if e.Critical {
			critical = append(critical, fmt.Sprint(i))
		}
	}
	if len(critical) > 0 {
		fmt.Fprintf(w, "  linkStyle %s stroke-width:3px\n", strings.Join(critical, ","))
	}
	if len(cyclic) > 0 {
		fmt.Fprintf(w, "  linkStyle %s stroke:red,stroke-width:3px\n", strings.Join(cyclic, ","))
	}
}
