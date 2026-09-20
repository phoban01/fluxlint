package graph

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func TestCyclesFindsPlantedCycle(t *testing.T) {
	g := New()
	g.AddEdge(Edge{From: "a", To: "b"})
	g.AddEdge(Edge{From: "b", To: "c"})
	g.AddEdge(Edge{From: "c", To: "a"})
	g.AddEdge(Edge{From: "c", To: "d"})
	cycles := g.Cycles()
	if len(cycles) != 1 || fmt.Sprint(cycles[0]) != "[a b c]" {
		t.Fatalf("cycles = %v", cycles)
	}
	if _, ok := g.Solve(); ok {
		t.Fatal("Solve must refuse a cyclic graph: A* does not exist")
	}
}

func TestSelfLoopIsACycle(t *testing.T) {
	g := New()
	g.AddEdge(Edge{From: "a", To: "a"})
	if len(g.Cycles()) != 1 {
		t.Fatal("self loop not reported")
	}
}

func TestSolveSlackAndCriticalPath(t *testing.T) {
	g := New()
	g.AddEdge(Edge{From: "s", To: "long", Weight: 10 * time.Minute})
	g.AddEdge(Edge{From: "s", To: "short", Weight: 1 * time.Minute})
	g.AddEdge(Edge{From: "long", To: "end"})
	g.AddEdge(Edge{From: "short", To: "end"})
	s, ok := g.Solve()
	if !ok {
		t.Fatal("acyclic graph not solved")
	}
	if s.Makespan != 10*time.Minute {
		t.Errorf("makespan = %v", s.Makespan)
	}
	if s.Slack("long") != 0 || s.Slack("short") != 9*time.Minute {
		t.Errorf("slack long=%v short=%v", s.Slack("long"), s.Slack("short"))
	}
	path := s.PathTo("end")
	if len(path) != 2 || path[0].To != "long" {
		t.Errorf("critical path = %v", path)
	}
	if m, _ := g.MakespanWithout(Edge{From: "s", To: "long", Weight: 10 * time.Minute}); m != time.Minute {
		t.Errorf("makespan without the long edge = %v", m)
	}
}

// Longest path must agree with brute-force enumeration on random DAGs.
func TestSolveMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		n := 2 + rng.Intn(7)
		g := New()
		for i := 0; i < n; i++ {
			g.AddNode(fmt.Sprint(i))
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ { // edges only forward: a DAG by construction
				if rng.Intn(3) == 0 {
					g.AddEdge(Edge{From: fmt.Sprint(i), To: fmt.Sprint(j), Weight: time.Duration(rng.Intn(10)) * time.Second})
				}
			}
		}
		s, ok := g.Solve()
		if !ok {
			t.Fatal("random DAG reported cyclic")
		}
		var brute func(v string) time.Duration
		brute = func(v string) time.Duration {
			var best time.Duration
			for _, e := range g.In(v) {
				best = max(best, brute(e.From)+e.Weight)
			}
			return best
		}
		for _, v := range g.Nodes() {
			if s.Earliest[v] != brute(v) {
				t.Fatalf("trial %d node %s: earliest %v, brute force %v", trial, v, s.Earliest[v], brute(v))
			}
			if s.Slack(v) < 0 {
				t.Fatalf("trial %d node %s: negative slack", trial, v)
			}
		}
	}
}

func TestReachableIgnoringAnEdge(t *testing.T) {
	g := New()
	direct := Edge{From: "c", To: "a"}
	g.AddEdge(direct)
	g.AddEdge(Edge{From: "c", To: "b"})
	g.AddEdge(Edge{From: "b", To: "a"})
	if !g.Reachable("c", "a", &direct) {
		t.Error("c reaches a through b even without the direct edge")
	}
	if g.Reachable("a", "c", nil) {
		t.Error("a must not reach c")
	}
}
