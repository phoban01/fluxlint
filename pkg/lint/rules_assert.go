package lint

import (
	"fmt"
	"path"

	"github.com/google/cel-go/cel"
	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

// assertionRules evaluates the repository's own invariants: CEL expressions
// over each matching rendered object, with the owning component's resolved
// post-build variables in scope. This is where conventions that only make
// sense for one repository live, so that the core stays general.
func (r *run) assertionRules() {
	if len(r.cfg.Assertions) == 0 {
		return
	}
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("vars", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("component", cel.StringType),
	)
	if err != nil {
		r.report("FL-A002", nil, nil, "cannot initialise CEL: "+err.Error())
		return
	}
	for _, a := range r.cfg.Assertions {
		ast, iss := env.Compile(a.Expr)
		if iss != nil && iss.Err() != nil {
			r.report("FL-A002", nil, nil, fmt.Sprintf("assertion %q does not compile: %v", a.Name, iss.Err()))
			continue
		}
		if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
			r.report("FL-A002", nil, nil, fmt.Sprintf("assertion %q must evaluate to a bool, not %s", a.Name, ast.OutputType()))
			continue
		}
		prg, err := env.Program(ast)
		if err != nil {
			r.report("FL-A002", nil, nil, fmt.Sprintf("assertion %q: %v", a.Name, err))
			continue
		}
		matched := false
		for _, c := range r.ix.Tree.Components {
			// A nested Kustomization's own spec (versions, replicas, image
			// tags) was substituted by the nearest ancestor with a postBuild,
			// so that is the scope an assertion about it needs.
			vars := map[string]string{}
			for x := c; x != nil; x = x.Parent {
				if x.Vars != nil {
					vars = x.Vars
					break
				}
			}
			for _, o := range c.Objects {
				if !matches(a.Match, c, o) {
					continue
				}
				matched = true
				out, _, err := prg.Eval(map[string]any{"object": jsonable(o), "vars": vars, "component": c.String()})
				sev := Severity(a.Severity)
				if sev == "" {
					sev = Error
				}
				switch {
				case err != nil:
					// a silent pass would be worse than a loud failure
					r.reportAs(sev, "FL-A001", c, o, fmt.Sprintf("%s: could not evaluate: %v", a.Name, err), "expr: "+a.Expr)
				case out.Value() != true:
					msg := a.Message
					if msg == "" {
						msg = "assertion failed"
					}
					r.reportAs(sev, "FL-A001", c, o, a.Name+": "+msg, "expr: "+a.Expr)
				}
			}
		}
		if !matched && a.MustMatch {
			r.report("FL-A001", nil, nil, fmt.Sprintf("%s: no rendered object matches %+v, so the assertion guards nothing", a.Name, a.Match))
		}
	}
}

func matches(m config.Match, c *model.Component, o model.Object) bool {
	glob := func(pattern, value string) bool {
		if pattern == "" {
			return true
		}
		ok, err := path.Match(pattern, value)
		return err == nil && ok
	}
	return glob(m.Kind, o.Kind()) && glob(m.Group, o.Group()) && glob(m.Namespace, o.Namespace()) &&
		glob(m.Name, o.Name()) && glob(m.Component, c.String())
}
