// Package lint runs fluxlint's rules over a rendered tree.
package lint

import (
	"sort"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Info    Severity = "info"
	Off     Severity = "off"
)

func (s Severity) rank() int {
	switch s {
	case Error:
		return 0
	case Warning:
		return 1
	default:
		return 2
	}
}

// Rule describes one check. IDs are stable.
type Rule struct {
	ID       string
	Name     string
	Severity Severity
	Help     string
}

// Finding is one reported problem or suggestion.
type Finding struct {
	Rule       string   `json:"rule"`
	Name       string   `json:"name"`
	Severity   Severity `json:"severity"`
	Entrypoint string   `json:"entrypoint"`
	Component  string   `json:"component,omitempty"`
	Object     string   `json:"object,omitempty"`
	Message    string   `json:"message"`
	Detail     []string `json:"detail,omitempty"`
}

// Rules is the catalogue, in report order.
var Rules = []Rule{
	{"FL-G001", "dangling-reference", Error, "A dependsOn or sourceRef names an object that nothing in the repository defines. Flux reports the dependent as not ready forever."},
	{"FL-G002", "bootstrap-deadlock", Error, "The precedence graph has a cycle, so the repository cannot converge from an empty cluster. Each edge of the cycle is listed with its reason; remove or reverse one."},
	{"FL-G003", "dual-ownership", Error, "The same object is rendered by more than one Flux Kustomization. They overwrite each other on every reconcile, and pruning one deletes the object for both."},
	{"FL-G004", "missing-namespace", Error, "An object is applied into a namespace that nothing creates. Declare it under externals.namespaces if it is created out of band."},
	{"FL-G005", "unknown-crd", Warning, "Custom resources of this API group are applied but no rendered component installs its CRDs. They may come from a Helm chart or external source fluxlint has not rendered; declare the group under externals.crdGroups to acknowledge."},
	{"FL-G008", "build-failed", Error, "kustomize build (or post-build substitution) failed for the path a Flux Kustomization points at."},
	{"FL-S001", "undefined-variable", Error, "A ${var} has no definition in postBuild.substitute / substituteFrom and no default. Flux substitutes the empty string."},
	{"FL-S002", "missing-substitute-source", Error, "postBuild.substituteFrom names a ConfigMap/Secret that nothing renders and that is not optional. Declare it under externals.substitutions if it is created out of band."},
	{"FL-S004", "unexpanded-variable", Info, "${...} expressions appear in a Kustomization without postBuild, so they reach the cluster verbatim. Expected for shell snippets; a bug if Flux substitution was intended."},
	{"FL-X001", "source-unavailable", Warning, "A Kustomization reads from another repository or artifact that could not be materialised, so nothing it applies was analysed. Run without --offline, fix access, or map it with sources.overrides."},
	{"FL-X002", "floating-ref", Info, "A source follows a branch or semver range. What Flux applies can change without a commit to this repository, and fluxlint's result reflects whatever was fetched last."},
	{"FL-T001", "critical-path", Info, "The chain of waits that determines the worst-case time for a bootstrap to converge. A bound, not a prediction."},
	{"FL-T002", "dominant-delay", Info, "One component contributes a large share of the worst-case bound, usually a generous timeout underneath a wait: true parent."},
	{"FL-T004", "unjustified-dependency", Info, "A dependsOn edge for which no import exists between the two subtrees. It may encode a runtime need fluxlint cannot see; if not, it only serialises reconciliation."},
	{"FL-T005", "redundant-dependency", Info, "A dependsOn edge already implied by another dependsOn path."},
	{"FL-T006", "implicit-ordering", Warning, "A component imports something from another without any declared ordering. It converges only by failing and retrying."},
	{"FL-T007", "retry-cliff", Warning, "No retryInterval and a long interval: one failed apply stalls the component for a full interval."},
	{"FL-T008", "timeout-inversion", Warning, "A wait: true parent times out before the components it waits for are allowed to, so it flaps NotReady while they legitimately converge."},
	{"FL-T100", "bootstrap-budget", Error, "The worst-case bootstrap bound exceeds timing.maxBootstrapBound."},
}

// RuleByID looks a rule up by ID or name.
func RuleByID(id string) (Rule, bool) {
	for _, r := range Rules {
		if r.ID == id || r.Name == id {
			return r, true
		}
	}
	return Rule{}, false
}

type run struct {
	ix       *Index
	cfg      *config.Config
	findings []Finding
}

func (r *run) report(id string, c *model.Component, o model.Object, msg string, detail ...string) {
	rule, _ := RuleByID(id)
	sev := rule.Severity
	if s, ok := r.cfg.Rules[id]; ok {
		sev = Severity(s)
	}
	if sev == Off {
		return
	}
	f := Finding{Rule: id, Name: rule.Name, Severity: sev, Entrypoint: r.ix.Tree.Entrypoint, Message: msg, Detail: detail}
	if c != nil {
		f.Component = c.String()
	}
	if o != nil {
		f.Object = o.String()
	}
	r.findings = append(r.findings, f)
}

// Result of analysing one entrypoint.
type Result struct {
	Tree     *model.Tree
	Findings []Finding
	Timing   *Timing
}

// Run executes every rule.
func Run(t *model.Tree, cfg *config.Config) *Result {
	r := &run{ix: BuildIndex(t, cfg), cfg: cfg}
	r.graphRules()
	r.substitutionRules()
	timing := r.timingRules()
	sort.SliceStable(r.findings, func(i, j int) bool {
		a, b := r.findings[i], r.findings[j]
		if a.Severity.rank() != b.Severity.rank() {
			return a.Severity.rank() < b.Severity.rank()
		}
		return a.Rule < b.Rule
	})
	return &Result{Tree: t, Findings: r.findings, Timing: timing}
}
