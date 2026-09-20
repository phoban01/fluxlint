package lint

import (
	"time"

	"github.com/phoban01/fluxlint/pkg/graph"
	"github.com/phoban01/fluxlint/pkg/model"
)

// Every component contributes two events.
func startOf(c *model.Component) string { return "start(" + c.String() + ")" }
func readyOf(c *model.Component) string { return "ready(" + c.String() + ")" }

// healthBound is the longest a component may legitimately take to become Ready.
func healthBound(c *model.Component) time.Duration {
	if c.IsHelmRelease() {
		// helm-controller waits up to spec.timeout per attempt and retries
		// the install spec.install.remediation.retries times
		return c.Timeout * time.Duration(1+c.Retries)
	}
	if !c.BlocksOnHealth() {
		return 0
	}
	if c.HasTimeout {
		return c.Timeout
	}
	return c.Interval // Flux defaults spec.timeout to spec.interval
}

// declaredGraph holds only what the manifests state: nesting, wait and
// dependsOn. With weights it is the timed event graph of the repository.
func declaredGraph(ix *Index) *graph.Graph {
	g := graph.New()
	requeue := ix.Cfg.Timing.DependencyRequeue.Duration
	for _, c := range ix.Tree.Components {
		g.AddEdge(graph.Edge{From: startOf(c), To: readyOf(c), Weight: healthBound(c), Reason: "apply and health timeout"})
		if p := c.Parent; p != nil {
			g.AddEdge(graph.Edge{From: startOf(p), To: startOf(c), Reason: "created by parent"})
			if p.Wait {
				g.AddEdge(graph.Edge{From: readyOf(c), To: readyOf(p), Reason: "parent has wait: true"})
			}
		}
		for _, d := range c.DependsOn {
			if dep := ix.Tree.ByKey[c.DepKey(d)]; dep != nil {
				g.AddEdge(graph.Edge{From: readyOf(dep), To: startOf(c), Weight: requeue, Reason: "dependsOn"})
			}
		}
	}
	return g
}

// needEdge is the precedence an import implies.
func needEdge(n Need) graph.Edge {
	reason := string(n.Kind) + " " + n.What + " needed by " + n.Object.String()
	switch n.Kind {
	case NeedCRD:
		// the CRD must be established, and server-side dry-run is
		// all-or-nothing per Kustomization
		return graph.Edge{From: readyOf(n.Provider), To: startOf(n.Consumer), Reason: reason}
	case NeedRuntime:
		// pods wait for their Secrets and ServiceAccounts; nothing fails
		return graph.Edge{From: startOf(n.Provider), To: readyOf(n.Consumer), Reason: reason}
	case NeedSource:
		if n.Consumer.IsHelmRelease() {
			// the HelmRelease object applies without its source, but the
			// release never becomes Ready
			return graph.Edge{From: startOf(n.Provider), To: readyOf(n.Consumer), Reason: reason}
		}
		return graph.Edge{From: startOf(n.Provider), To: startOf(n.Consumer), Reason: reason}
	default:
		return graph.Edge{From: startOf(n.Provider), To: startOf(n.Consumer), Reason: reason}
	}
}

// matters reports whether an unmet need can block convergence of the consumer.
func (n Need) matters() bool {
	if n.Kind == NeedRuntime {
		return n.Consumer.BlocksOnHealth() || n.Consumer.IsHelmRelease()
	}
	return true
}

// fullGraph adds the precedence implied by imports to the declared graph.
func fullGraph(ix *Index) *graph.Graph {
	g := declaredGraph(ix)
	for _, n := range ix.Needs {
		if n.matters() {
			g.AddEdge(needEdge(n))
		}
	}
	return g
}
