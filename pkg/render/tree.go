// Package render turns a Flux repository into a model.Tree without a cluster:
// in-process kustomize builds, Flux's Kustomization-level overlay and post-build
// substitution, recursing through nested Flux Kustomizations.
package render

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phoban01/fluxlint/pkg/source"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// Tree renders everything reachable from entrypoint (relative to repoRoot).
// A nil resolver leaves components with external sources unrendered.
func Tree(ctx context.Context, repoRoot, entrypoint string, cfg *config.Config, resolver *source.Resolver) (*model.Tree, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	root := &model.Component{
		Name:      entrypoint,
		Namespace: cfg.RepoSource.Namespace,
		IsRoot:    true,
		Path:      entrypoint,
		Prune:     true,
		// whatever applies the entrypoint reads this repository
		Source: model.Ref{Kind: cfg.RepoSource.Kind, Name: cfg.RepoSource.Name, Namespace: cfg.RepoSource.Namespace},
	}
	t := &model.Tree{RepoRoot: repoRoot, Entrypoint: entrypoint, Root: root, ByKey: map[string]*model.Component{}}
	r := &renderer{repoRoot: repoRoot, cfg: cfg, tree: t, resolver: resolver,
		data: map[string]map[string]string{}, sources: map[string]model.Object{}}

	// rawOrigins[c][i] is the file c.Raw[i] came from.
	rawOrigins := map[*model.Component][]string{}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, err
	}

	level := []*model.Component{root}
	for len(level) > 0 {
		// 0. materialise external sources, concurrently
		r.fetch(ctx, level)
		// 1. render every component of this level: charts concurrently,
		// kustomize builds one at a time (see buildMu)
		var helmWG sync.WaitGroup
		for _, c := range level {
			if c.Opaque != "" || !c.IsHelmRelease() {
				continue
			}
			spec := r.helmSpecOf(c.Spec)
			c.RenderNotes = spec.Notes
			helmWG.Add(1)
			go func() {
				defer helmWG.Done()
				ch, err := loader.Load(c.SourceRoot)
				var raw []model.Object
				if err == nil {
					c.RenderNotes = append(c.RenderNotes, addDependencies(ch, c.SourceRoot, func(repoURL, name, version string) (string, error) {
						return r.fetchChart(ctx, c.Namespace, repoURL, name, version)
					})...)
					var notes []string
					c.Contract, notes = chartContract(ch)
					c.RenderNotes = append(c.RenderNotes, notes...)
					raw, err = helmTemplate(ch, spec, cfg.KubeVersion)
				}
				if err != nil {
					c.BuildErr, c.Opaque = err, "chart rendering failed"
					return
				}
				c.Raw = raw
			}()
		}
		for _, c := range level {
			t.Components = append(t.Components, c)
			// Kind first: a HelmRelease's other fields belong to its render
			// goroutine until helmWG.Wait()
			if c.IsHelmRelease() || c.Opaque != "" {
				continue
			}
			base := repoRoot
			if c.External {
				base = c.SourceRoot
			}
			ov := overlayFromSpec(c.Spec)
			if !hasKustomization(filepath.Join(base, c.Path)) {
				ov.Ignored = ignoreFilter(base, r.sources[c.Source.String()])
			}
			raw, origins, err := build(filepath.Join(base, c.Path), ov)
			if err != nil {
				c.BuildErr, c.Opaque = err, "build failed"
				continue
			}
			c.Raw = raw
			if !c.External {
				rawOrigins[c] = origins
			}
			if c.IsRoot {
				r.adoptBootstrap(c)
				// the bootstrap Kustomization may itself carry patches/images
				if ov := overlayFromSpec(c.Spec); !ov.empty() {
					if raw, origins, err := build(filepath.Join(repoRoot, c.Path), ov); err != nil {
						c.BuildErr, c.Opaque = err, "build failed"
					} else {
						c.Raw, rawOrigins[c] = raw, origins
					}
				}
			}
		}
		helmWG.Wait()
		// 2. make this level's ConfigMaps/Secrets visible to substituteFrom
		for _, c := range level {
			r.indexData(c.Raw)
		}
		// 3. substitute, then discover the next level
		var next []*model.Component
		for _, c := range level {
			if c.Opaque != "" {
				continue
			}
			if err := r.substitute(c); err != nil {
				c.BuildErr, c.Opaque = err, "substitution failed"
				continue
			}
			r.indexData(c.Objects)
			// substitution preserves order, so origins still line up
			if origins := rawOrigins[c]; len(origins) == len(c.Objects) {
				c.Origins = map[string]string{}
				for i, o := range c.Objects {
					if rel, err := filepath.Rel(realRoot, origins[i]); err == nil && origins[i] != "" && !strings.HasPrefix(rel, "..") {
						c.Origins[o.ID()] = filepath.ToSlash(rel)
					}
				}
			}
			for _, o := range c.Objects {
				var child *model.Component
				switch {
				case o.IsFluxKustomization():
					child = r.newComponent(o, c)
				case o.IsHelmRelease():
					child = r.newHelmComponent(o, c)
				default:
					continue
				}
				if c.IsRoot && child.Key() == c.Key() {
					continue // the bootstrap Kustomization is the root itself
				}
				if _, dup := t.ByKey[child.Key()]; dup {
					continue // reported as dual ownership by the rules
				}
				t.ByKey[child.Key()] = child
				c.Children = append(c.Children, child)
				next = append(next, child)
			}
		}
		level = next
	}
	t.ByKey[root.Key()] = root
	return t, nil
}

type renderer struct {
	repoRoot string
	cfg      *config.Config
	tree     *model.Tree
	resolver *source.Resolver
	// data is "Kind/namespace/name" -> key/values, for substituteFrom.
	data map[string]map[string]string
	// sources is Ref.String() -> rendered Flux source object.
	sources map[string]model.Object
}

// fetch resolves the external source of every component in level.
func (r *renderer) fetch(ctx context.Context, level []*model.Component) {
	var wg sync.WaitGroup
	for _, c := range level {
		if !c.External {
			continue
		}
		src, ok := r.sources[c.Source.String()]
		switch {
		case !ok:
			c.Opaque = model.OpaqueUndefinedSource // FL-G001 reports it
		case r.resolver == nil:
			c.Opaque = model.OpaqueNoResolver
		default:
			wg.Add(1)
			go func() {
				defer wg.Done()
				var res *source.Result
				var err error
				if c.IsHelmRelease() {
					res, err = r.resolver.Chart(ctx, c.Spec, src)
				} else {
					res, err = r.resolver.Resolve(ctx, src)
				}
				if err != nil {
					c.SourceErr = err
					c.Opaque = "source " + c.Source.String() + " unavailable"
					if errors.Is(err, source.ErrUnsupported) {
						c.Opaque = "source kind " + c.Source.Kind + " is not supported yet"
					}
					return
				}
				c.SourceRoot, c.SourceRevision, c.FloatingRef = res.Dir, res.Revision, res.Floating
				inspectSource(c)
			}()
		}
	}
	wg.Wait()
}

func (r *renderer) indexData(objs []model.Object) {
	for _, o := range objs {
		if k := o.Kind(); (k == "ConfigMap" || k == "Secret") && o.Group() == "" {
			r.data[k+"/"+o.Namespace()+"/"+o.Name()] = dataOf(o)
		}
		if o.IsSource() {
			ref := model.Ref{Kind: o.Kind(), Namespace: o.Namespace(), Name: o.Name()}
			r.sources[ref.String()] = o
		}
	}
}

// adoptBootstrap finds the Flux Kustomization that applies the entrypoint
// itself (as written by `flux bootstrap`) and gives the root its identity and
// settings.
func (r *renderer) adoptBootstrap(root *model.Component) {
	for _, o := range root.Raw {
		if !o.IsFluxKustomization() {
			continue
		}
		c := r.newComponent(o, nil)
		if !c.External && filepath.Clean(c.Path) == filepath.Clean(root.Path) {
			c.IsRoot, c.Raw = true, root.Raw
			*root = *c
			return
		}
	}
}

func (r *renderer) isRepoSource(ref model.Ref) bool {
	s := r.cfg.RepoSource
	return ref.Kind == s.Kind && ref.Name == s.Name && ref.Namespace == s.Namespace
}

func (r *renderer) newComponent(o model.Object, parent *model.Component) *model.Component {
	c := &model.Component{
		Namespace: o.Namespace(),
		Name:      o.Name(),
		Parent:    parent,
		Spec:      o,
		Path:      model.Str(o, "spec", "path"),
		Wait:      model.Bool(o, "spec", "wait"),
		Prune:     model.Bool(o, "spec", "prune"),
		Health:    len(model.List(o, "spec", "healthChecks")) > 0,
	}
	c.Source = model.Ref{
		Kind:      model.Str(o, "spec", "sourceRef", "kind"),
		Name:      model.Str(o, "spec", "sourceRef", "name"),
		Namespace: model.Str(o, "spec", "sourceRef", "namespace"),
	}
	if c.Source.Namespace == "" {
		c.Source.Namespace = c.Namespace
	}
	for _, d := range model.List(o, "spec", "dependsOn") {
		ref := model.Ref{Kind: "Kustomization", Name: model.Str(d, "name"), Namespace: model.Str(d, "namespace")}
		if ref.Namespace == "" {
			ref.Namespace = c.Namespace
		}
		c.DependsOn = append(c.DependsOn, ref)
	}
	c.Interval, _ = parseDuration(model.Str(o, "spec", "interval"))
	c.RetryInterval, c.HasRetryInterval = parseDuration(model.Str(o, "spec", "retryInterval"))
	c.Timeout, c.HasTimeout = parseDuration(model.Str(o, "spec", "timeout"))

	if pb := model.Get(o, "spec", "postBuild"); pb != nil {
		c.HasPostBuild = true
		c.Substitute = map[string]string{}
		if m, ok := model.Get(pb, "substitute").(map[string]any); ok {
			for k, v := range m {
				c.Substitute[k] = fmt.Sprint(v)
			}
		}
		for _, sf := range model.List(pb, "substituteFrom") {
			c.SubstituteFrom = append(c.SubstituteFrom, model.SubstituteRef{
				Kind: model.Str(sf, "kind"), Name: model.Str(sf, "name"), Optional: model.Bool(sf, "optional"),
			})
		}
	}
	c.External = !r.isRepoSource(c.Source)
	return c
}

func (r *renderer) newHelmComponent(o model.Object, parent *model.Component) *model.Component {
	c := &model.Component{
		Kind:      model.KindHelmRelease,
		Namespace: o.Namespace(),
		Name:      o.Name(),
		Parent:    parent,
		Spec:      o,
		External:  true, // a chart is always fetched
		Prune:     true,
		Wait:      !model.Bool(o, "spec", "install", "disableWait"),
	}
	at := model.Get(o, "spec", "chartRef")
	if at == nil {
		at = model.Get(o, "spec", "chart", "spec", "sourceRef")
	}
	c.Source = model.Ref{Kind: model.Str(at, "kind"), Name: model.Str(at, "name"), Namespace: model.Str(at, "namespace")}
	if c.Source.Namespace == "" {
		c.Source.Namespace = c.Namespace
	}
	for _, d := range model.List(o, "spec", "dependsOn") {
		ref := model.Ref{Kind: model.KindHelmRelease, Name: model.Str(d, "name"), Namespace: model.Str(d, "namespace")}
		if ref.Namespace == "" {
			ref.Namespace = c.Namespace
		}
		c.DependsOn = append(c.DependsOn, ref)
	}
	c.Interval, _ = parseDuration(model.Str(o, "spec", "interval"))
	if c.Timeout, c.HasTimeout = parseDuration(model.Str(o, "spec", "timeout")); !c.HasTimeout {
		c.Timeout, c.HasTimeout = 5*time.Minute, true // helm-controller's default
	}
	switch n := model.Get(o, "spec", "install", "remediation", "retries").(type) {
	case int:
		c.Retries = n
	case int64:
		c.Retries = int(n)
	case float64:
		c.Retries = int(n)
	}
	if model.Bool(o, "spec", "install", "createNamespace") {
		c.CreatesNamespace = c.Namespace
		if tn := model.Str(o, "spec", "targetNamespace"); tn != "" {
			c.CreatesNamespace = tn
		}
	}
	return c
}

func parseDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil
}

func (r *renderer) substitute(c *model.Component) error {
	if !c.HasPostBuild {
		c.Objects = c.Raw
		if !c.IsHelmRelease() { // chart output is full of shell and template text
			c.LiteralVars = literalVars(c.Raw)
		}
		return nil
	}
	vars := map[string]string{}
	for _, sf := range c.SubstituteFrom {
		if data, ok := r.data[sf.Kind+"/"+c.Namespace+"/"+sf.Name]; ok {
			for k, v := range data {
				vars[k] = v
			}
			continue
		}
		if ext := r.external(sf); ext != nil {
			for _, v := range ext.Variables {
				if _, set := vars[v]; !set {
					vars[v] = "fluxlint-external-" + v
				}
			}
			continue
		}
		if !sf.Optional {
			c.MissingSubstituteFrom = append(c.MissingSubstituteFrom, sf)
		}
	}
	for k, v := range c.Substitute { // inline values win
		vars[k] = v
	}
	c.Vars = vars
	objs, uses, err := substitute(c.Raw, vars)
	if err != nil {
		return err
	}
	c.Objects, c.VarUses = objs, uses
	return nil
}

func (r *renderer) external(sf model.SubstituteRef) *config.ExternalSubstitution {
	for i, e := range r.cfg.Externals.Substitutions {
		if e.Kind == sf.Kind && e.Name == sf.Name {
			return &r.cfg.Externals.Substitutions[i]
		}
	}
	return nil
}
