// Package render turns a Flux repository into a model.Tree without a cluster:
// in-process kustomize builds, Flux's Kustomization-level overlay and post-build
// substitution, recursing through nested Flux Kustomizations.
package render

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

// Tree renders everything reachable from entrypoint (relative to repoRoot).
func Tree(repoRoot, entrypoint string, cfg *config.Config) (*model.Tree, error) {
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
	}
	t := &model.Tree{Entrypoint: entrypoint, Root: root, ByKey: map[string]*model.Component{}}
	r := &renderer{repoRoot: repoRoot, cfg: cfg, tree: t, data: map[string]map[string]string{}}

	level := []*model.Component{root}
	for len(level) > 0 {
		// 1. kustomize build every component of this level
		for _, c := range level {
			t.Components = append(t.Components, c)
			if c.Opaque != "" {
				continue
			}
			raw, err := build(filepath.Join(repoRoot, c.Path), overlayFromSpec(c.Spec))
			if err != nil {
				c.BuildErr, c.Opaque = err, "build failed"
				continue
			}
			c.Raw = raw
			if c.IsRoot {
				r.adoptBootstrap(c)
				// the bootstrap Kustomization may itself carry patches/images
				if ov := overlayFromSpec(c.Spec); !ov.empty() {
					if raw, err := build(filepath.Join(repoRoot, c.Path), ov); err != nil {
						c.BuildErr, c.Opaque = err, "build failed"
					} else {
						c.Raw = raw
					}
				}
			}
		}
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
			for _, o := range c.Objects {
				if !o.IsFluxKustomization() {
					continue
				}
				child := r.newComponent(o, c)
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
	// data is "Kind/namespace/name" -> key/values, for substituteFrom.
	data map[string]map[string]string
}

func (r *renderer) indexData(objs []model.Object) {
	for _, o := range objs {
		if k := o.Kind(); (k == "ConfigMap" || k == "Secret") && o.Group() == "" {
			r.data[k+"/"+o.Namespace()+"/"+o.Name()] = dataOf(o)
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
		if c.Opaque == "" && filepath.Clean(c.Path) == filepath.Clean(root.Path) {
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
	if !r.isRepoSource(c.Source) {
		c.Opaque = "external source " + c.Source.String()
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
		c.LiteralVars = literalVars(c.Raw)
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
