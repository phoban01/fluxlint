// Package graph is a small weighted digraph with the algorithms fluxlint needs:
// strongly connected components, max-plus longest paths with slack, and
// redundancy of edges under reachability.
package graph

import (
	"sort"
	"time"
)

// Edge means "From cannot happen after To starts": To >= From + Weight.
type Edge struct {
	From, To string
	Weight   time.Duration
	Reason   string
}

// Graph is a directed multigraph over string node IDs.
type Graph struct {
	nodes map[string]bool
	order []string
	out   map[string][]Edge
	in    map[string][]Edge
}

func New() *Graph {
	return &Graph{nodes: map[string]bool{}, out: map[string][]Edge{}, in: map[string][]Edge{}}
}

func (g *Graph) AddNode(n string) {
	if !g.nodes[n] {
		g.nodes[n] = true
		g.order = append(g.order, n)
	}
}

func (g *Graph) AddEdge(e Edge) {
	g.AddNode(e.From)
	g.AddNode(e.To)
	g.out[e.From] = append(g.out[e.From], e)
	g.in[e.To] = append(g.in[e.To], e)
}

func (g *Graph) Nodes() []string     { return append([]string(nil), g.order...) }
func (g *Graph) Out(n string) []Edge { return g.out[n] }
func (g *Graph) In(n string) []Edge  { return g.in[n] }

// Cycles returns every strongly connected component with more than one node,
// or a single node with a self-loop (Tarjan, iterative-safe sizes assumed small).
func (g *Graph) Cycles() [][]string {
	index, low := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string
	next := 0
	var visit func(v string)
	visit = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, e := range g.out[v] {
			if _, seen := index[e.To]; !seen {
				visit(e.To)
				low[v] = min(low[v], low[e.To])
			} else if onStack[e.To] {
				low[v] = min(low[v], index[e.To])
			}
		}
		if low[v] != index[v] {
			return
		}
		var scc []string
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			scc = append(scc, w)
			if w == v {
				break
			}
		}
		if len(scc) > 1 || g.hasSelfLoop(v) {
			sort.Strings(scc)
			out = append(out, scc)
		}
	}
	for _, n := range g.order {
		if _, seen := index[n]; !seen {
			visit(n)
		}
	}
	return out
}

func (g *Graph) hasSelfLoop(n string) bool {
	for _, e := range g.out[n] {
		if e.To == n {
			return true
		}
	}
	return false
}

// EdgesWithin returns the edges whose endpoints are both in set.
func (g *Graph) EdgesWithin(set []string) []Edge {
	in := map[string]bool{}
	for _, n := range set {
		in[n] = true
	}
	var out []Edge
	for _, n := range set {
		for _, e := range g.out[n] {
			if in[e.To] {
				out = append(out, e)
			}
		}
	}
	return out
}

// Reachable reports whether to can be reached from from, optionally ignoring
// one edge (matched by From, To and Reason).
func (g *Graph) Reachable(from, to string, ignore *Edge) bool {
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, e := range g.out[v] {
			if ignore != nil && e.From == ignore.From && e.To == ignore.To && e.Reason == ignore.Reason {
				continue
			}
			if e.To == to {
				return true
			}
			if !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	return false
}

// Schedule is the max-plus solution x = A* (x) b of an acyclic graph.
type Schedule struct {
	Earliest map[string]time.Duration
	Latest   map[string]time.Duration
	Pred     map[string]*Edge // the binding predecessor edge of each node
	Makespan time.Duration
	End      string // a node that attains the makespan
}

// Slack is how far a node can slip without moving the makespan.
func (s *Schedule) Slack(n string) time.Duration { return s.Latest[n] - s.Earliest[n] }

// CriticalPath returns the binding chain of edges that ends at End, in time order.
func (s *Schedule) CriticalPath() []Edge { return s.PathTo(s.End) }

// PathTo returns the binding chain of edges that ends at n, in time order.
func (s *Schedule) PathTo(n string) []Edge {
	var path []Edge
	for e := s.Pred[n]; e != nil; e = s.Pred[e.From] {
		path = append([]Edge{*e}, path...)
	}
	return path
}

// Solve computes earliest and latest event times. ok is false when the graph
// has a cycle, in which case A* does not exist.
func (g *Graph) Solve() (s *Schedule, ok bool) {
	topo, ok := g.topo()
	if !ok {
		return nil, false
	}
	s = &Schedule{Earliest: map[string]time.Duration{}, Latest: map[string]time.Duration{}, Pred: map[string]*Edge{}}
	for _, v := range topo {
		for i := range g.in[v] {
			e := &g.in[v][i]
			if t := s.Earliest[e.From] + e.Weight; s.Pred[v] == nil || t > s.Earliest[v] {
				s.Earliest[v], s.Pred[v] = max(t, s.Earliest[v]), e
			}
		}
		if s.End == "" || s.Earliest[v] > s.Makespan {
			s.Makespan, s.End = s.Earliest[v], v
		}
	}
	for i := len(topo) - 1; i >= 0; i-- {
		v := topo[i]
		s.Latest[v] = s.Makespan
		for _, e := range g.out[v] {
			s.Latest[v] = min(s.Latest[v], s.Latest[e.To]-e.Weight)
		}
	}
	return s, true
}

// MakespanWithout is the makespan when one edge is removed.
func (g *Graph) MakespanWithout(drop Edge) (time.Duration, bool) {
	h := New()
	for _, n := range g.order {
		h.AddNode(n)
		for _, e := range g.out[n] {
			if e != drop {
				h.AddEdge(e)
			}
		}
	}
	s, ok := h.Solve()
	if !ok {
		return 0, false
	}
	return s.Makespan, true
}

func (g *Graph) topo() ([]string, bool) {
	indeg := map[string]int{}
	for _, n := range g.order {
		indeg[n] += 0
		for _, e := range g.out[n] {
			indeg[e.To]++
		}
	}
	var queue, out []string
	for _, n := range g.order {
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		out = append(out, v)
		for _, e := range g.out[v] {
			if indeg[e.To]--; indeg[e.To] == 0 {
				queue = append(queue, e.To)
			}
		}
	}
	return out, len(out) == len(g.order)
}
