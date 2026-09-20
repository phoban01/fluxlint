package lint

import (
	"fmt"
	"sort"
	"time"

	"github.com/phoban01/fluxlint/pkg/graph"
	"github.com/phoban01/fluxlint/pkg/model"
)

// Timing is the max-plus analysis of the declared graph.
type Timing struct {
	// Bound is the worst case for a bootstrap that succeeds: every health
	// wait runs to its timeout and every dependent polls a full requeue period.
	Bound        time.Duration   `json:"bound"`
	CriticalPath []PathStep      `json:"criticalPath"`
	Schedule     *graph.Schedule `json:"-"`
}

type PathStep struct {
	Event  string        `json:"event"`
	At     time.Duration `json:"at"`
	Added  time.Duration `json:"added"`
	Reason string        `json:"reason"`
}

const retryCliff = 10 * time.Minute

func (r *run) timingRules() *Timing {
	ix := r.ix
	g := declaredGraph(ix)
	s, ok := g.Solve()
	if !ok {
		// the declared graph itself is cyclic; FL-G002 already says so
		return nil
	}
	t := &Timing{Bound: s.Makespan, Schedule: s}
	var detail []string
	for _, e := range s.CriticalPath() {
		t.CriticalPath = append(t.CriticalPath, PathStep{Event: e.To, At: s.Earliest[e.To], Added: e.Weight, Reason: e.Reason})
		detail = append(detail, fmt.Sprintf("t<=%-8v %-45s +%-7v %s", s.Earliest[e.To], e.To, e.Weight, e.Reason))
	}
	r.report("FL-T001", nil, nil, fmt.Sprintf("worst-case bootstrap bound is %v", s.Makespan), detail...)

	if max := r.cfg.Timing.MaxBootstrapBound.Duration; max > 0 && s.Makespan > max {
		r.report("FL-T100", nil, nil, fmt.Sprintf("worst-case bootstrap bound %v exceeds the budget of %v", s.Makespan, max))
	}

	// dominant delays on the critical path
	if s.Makespan > 0 {
		for _, e := range s.CriticalPath() {
			if share := float64(e.Weight) / float64(s.Makespan); share >= r.cfg.Timing.DominantShare {
				c := r.componentOfEvent(e.To)
				r.report("FL-T002", c, nil, fmt.Sprintf("timeout of %v is %.0f%% of the worst-case bound (%v) and sits on the critical path",
					e.Weight, share*100, s.Makespan), r.blockedBy(c)...)
			}
		}
	}

	r.dependencyReview(g, s)

	// implicit ordering
	for _, n := range ix.Needs {
		if !n.matters() || n.Kind == NeedRuntime { // a pod waiting for a Secret is not a failed apply
			continue
		}
		e := needEdge(n)
		if g.Reachable(e.From, e.To, nil) || g.Reachable(e.To, e.From, nil) {
			continue // ordered already, or a deadlock that FL-G002 reports
		}
		penalty := n.Consumer.RetryInterval
		if !n.Consumer.HasRetryInterval {
			penalty = n.Consumer.Interval
		}
		r.report("FL-T006", n.Consumer, n.Object,
			fmt.Sprintf("needs %s %s from %s but no ordering is declared; each failed attempt costs %v", n.Kind, n.What, n.Provider, penalty),
			fmt.Sprintf("add dependsOn: %s to %s (or to the nearest ancestors that are siblings)", n.Provider, n.Consumer))
	}

	for _, c := range ix.Tree.Components {
		if !c.IsRoot && !c.IsHelmRelease() && !c.HasRetryInterval && c.Interval >= retryCliff {
			r.report("FL-T007", c, nil, fmt.Sprintf("no retryInterval: a failed apply is not retried for %v (interval)", c.Interval))
		}
		// timeout inversion: a waiting parent gives up before a child is allowed to
		pb := healthBound(c)
		if !c.Wait || pb == 0 {
			continue
		}
		var worst *model.Component
		var worstAllowed time.Duration
		late := 0
		for _, child := range c.Children {
			if !child.BlocksOnHealth() {
				continue
			}
			if allowed := s.Earliest[readyOf(child)] - s.Earliest[startOf(c)]; allowed > pb {
				late++
				if allowed > worstAllowed {
					worst, worstAllowed = child, allowed
				}
			}
		}
		if worst != nil {
			r.report("FL-T008", c, nil, fmt.Sprintf("wait: true with a %v timeout, but %d of its children may legitimately take longer; the slowest, %s, up to %v after the parent starts",
				pb, late, worst, worstAllowed))
		}
	}
	return t
}

func (r *run) componentOfEvent(ev string) *model.Component {
	for _, c := range r.ix.Tree.Components {
		if startOf(c) == ev || readyOf(c) == ev {
			return c
		}
	}
	return nil
}

// blockedBy lists what transitively waits for c through wait: true parents.
func (r *run) blockedBy(c *model.Component) []string {
	if c == nil {
		return nil
	}
	var out []string
	for p := c.Parent; p != nil && p.Wait; p = p.Parent {
		var deps []string
		for _, x := range r.ix.Tree.Components {
			for _, d := range x.DependsOn {
				if x.DepKey(d) == p.Key() {
					deps = append(deps, x.String())
				}
			}
		}
		sort.Strings(deps)
		out = append(out, fmt.Sprintf("%s has wait: true, so it is not Ready until %s is; dependents of %s: %v", p, c, p, deps))
	}
	return out
}

func (r *run) dependencyReview(g *graph.Graph, s *graph.Schedule) {
	ix := r.ix
	// dependsOn-only graph for transitive redundancy
	dg := graph.New()
	for _, c := range ix.Tree.Components {
		for _, d := range c.DependsOn {
			dg.AddEdge(graph.Edge{From: c.Key(), To: c.DepKey(d)})
		}
	}
	for _, c := range ix.Tree.Components {
		for _, d := range c.DependsOn {
			dep := ix.Tree.ByKey[c.DepKey(d)]
			if dep == nil {
				continue
			}
			if dg.Reachable(c.Key(), dep.Key(), &graph.Edge{From: c.Key(), To: dep.Key()}) {
				r.report("FL-T005", c, nil, fmt.Sprintf("dependsOn %s is already implied by another dependsOn path", dep))
				continue
			}
			if r.justified(dep, c) {
				continue
			}
			edge := graph.Edge{From: readyOf(dep), To: startOf(c), Weight: r.cfg.Timing.DependencyRequeue.Duration, Reason: "dependsOn"}
			msg := fmt.Sprintf("dependsOn %s, but nothing rendered in %s imports anything from %s", dep, c, dep)
			var detail []string
			if without, ok := g.MakespanWithout(edge); ok && without < s.Makespan {
				detail = append(detail, fmt.Sprintf("on the critical path: removing it lowers the worst-case bound by %v", s.Makespan-without))
			} else {
				detail = append(detail, fmt.Sprintf("not on the critical path (slack %v)", s.Slack(startOf(c))))
			}
			if n := r.opaqueWithin(c) + r.opaqueWithin(dep); n > 0 {
				detail = append(detail, fmt.Sprintf("low confidence: %d component(s) on either side are not rendered, so their imports and exports are unknown", n))
			}
			r.report("FL-T004", c, nil, msg, detail...)
		}
	}
}

// justified reports whether anything in consumer's subtree imports from
// provider's subtree.
func (r *run) justified(provider, consumer *model.Component) bool {
	for _, n := range r.ix.Needs {
		if n.Provider.Within(provider) && n.Consumer.Within(consumer) {
			return true
		}
	}
	return false
}

// opaqueWithin counts unrendered components in root's subtree.
func (r *run) opaqueWithin(root *model.Component) int {
	n := 0
	for _, c := range r.ix.Tree.Components {
		if c.Opaque != "" && c.Within(root) {
			n++
		}
	}
	return n
}
