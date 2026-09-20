// Package model holds the rendered view of a Flux repository: components
// (Flux Kustomizations), the objects each one applies, and helpers to read them.
package model

import (
	"strings"
	"time"
)

// Object is one rendered Kubernetes object.
type Object map[string]any

// Get walks nested maps and returns nil when any segment is missing.
func Get(o any, path ...string) any {
	for _, p := range path {
		switch m := o.(type) {
		case Object:
			o = m[p]
		case map[string]any:
			o = m[p]
		default:
			return nil
		}
	}
	return o
}

// Str returns the string at path, or "".
func Str(o any, path ...string) string { s, _ := Get(o, path...).(string); return s }

// Bool returns the bool at path, or false.
func Bool(o any, path ...string) bool { b, _ := Get(o, path...).(bool); return b }

// List returns the slice at path, or nil.
func List(o any, path ...string) []any { l, _ := Get(o, path...).([]any); return l }

func (o Object) APIVersion() string { return Str(o, "apiVersion") }
func (o Object) Kind() string       { return Str(o, "kind") }
func (o Object) Name() string       { return Str(o, "metadata", "name") }
func (o Object) Namespace() string  { return Str(o, "metadata", "namespace") }

// Group is the API group ("" for core).
func (o Object) Group() string {
	if g, _, ok := strings.Cut(o.APIVersion(), "/"); ok {
		return g
	}
	return ""
}

// Version is the API version without the group.
func (o Object) Version() string {
	if _, v, ok := strings.Cut(o.APIVersion(), "/"); ok {
		return v
	}
	return o.APIVersion()
}

// GK is "group/Kind".
func (o Object) GK() string { return o.Group() + "/" + o.Kind() }

// ID uniquely identifies the object in a cluster, ignoring API version.
func (o Object) ID() string {
	return o.GK() + "/" + o.Namespace() + "/" + o.Name()
}

// String is a human readable reference: Kind/ns/name or Kind/name.
func (o Object) String() string {
	if ns := o.Namespace(); ns != "" {
		return o.Kind() + "/" + ns + "/" + o.Name()
	}
	return o.Kind() + "/" + o.Name()
}

const (
	FluxKustomizeGroup = "kustomize.toolkit.fluxcd.io"
	FluxSourceGroup    = "source.toolkit.fluxcd.io"
	FluxHelmGroup      = "helm.toolkit.fluxcd.io"
)

// IsFluxKustomization reports whether o is a Flux Kustomization.
func (o Object) IsFluxKustomization() bool {
	return o.Group() == FluxKustomizeGroup && o.Kind() == "Kustomization"
}

// IsHelmRelease reports whether o is a Flux HelmRelease.
func (o Object) IsHelmRelease() bool { return o.Group() == FluxHelmGroup && o.Kind() == "HelmRelease" }

// IsSource reports whether o is a Flux source object.
func (o Object) IsSource() bool { return o.Group() == FluxSourceGroup }

// Ref names another namespaced object.
type Ref struct{ Kind, Namespace, Name string }

func (r Ref) String() string { return r.Kind + "/" + r.Namespace + "/" + r.Name }

// Component is a Flux Kustomization together with what it renders to. The
// synthetic root component (IsRoot) stands for the entrypoint directory applied
// by the bootstrap Kustomization.
type Component struct {
	Kind            string // KindKustomization or KindHelmRelease
	Namespace, Name string

	// HelmRelease only.
	Retries          int      // install remediation retries
	CreatesNamespace string   // install.createNamespace target: created if absent, not owned
	RenderNotes      []string // parts of the spec the renderer does not model yet
	IsRoot           bool
	Parent           *Component
	Children         []*Component
	Spec             Object // the Kustomization object as applied by Parent (post substitution)

	Path      string
	Source    Ref
	DependsOn []Ref
	Wait      bool
	Health    bool // has spec.healthChecks
	Prune     bool

	Interval, RetryInterval, Timeout time.Duration
	HasRetryInterval, HasTimeout     bool

	HasPostBuild   bool
	Substitute     map[string]string
	SubstituteFrom []SubstituteRef

	// External is set when the source is not the repository under analysis.
	// SourceRoot is then where its artifact was materialised.
	External       bool
	SourceRoot     string
	SourceRevision string
	FloatingRef    string // why the source ref is not reproducible, if so
	SourceErr      error

	// GoModule and GoRequires come from go.mod at the root of an external
	// source; Contract from its fluxlint-contract.yaml.
	GoModule   string
	GoRequires map[string]string
	Contract   *Contract

	// Opaque is non-empty when the component could not be rendered; the value
	// says why (external source, build error).
	Opaque   string
	BuildErr error

	// Raw is the kustomize output before substitution; Objects is after.
	Raw     []Object
	Objects []Object

	// Origins maps an object ID to the repository-relative file it was
	// rendered from, when that file is part of the repository under analysis.
	Origins map[string]string

	// Vars are the resolved post-build variables (substituteFrom merged with
	// inline substitute), when the component has a postBuild.
	Vars map[string]string

	// VarUses are the variable lookups made while substituting (HasPostBuild),
	// LiteralVars the ${...} texts left untouched (no postBuild).
	VarUses     []VarUse
	LiteralVars []VarUse
	// MissingSubstituteFrom are non-optional substituteFrom references that
	// nothing rendered so far, and no declared external, provides.
	MissingSubstituteFrom []SubstituteRef
}

// Opaque reasons that are not failures to fetch.
const (
	OpaqueNoResolver      = "source resolution is disabled"
	OpaqueUndefinedSource = "its source object is not defined"
)

// SubstituteRef is one postBuild.substituteFrom entry.
type SubstituteRef struct {
	Kind, Name string
	Optional   bool
}

// Component kinds. The zero value is a Flux Kustomization.
const (
	KindKustomization = ""
	KindHelmRelease   = "HelmRelease"
)

// Key is "namespace/name" for Kustomizations and "HelmRelease/namespace/name"
// for HelmReleases, which commonly share a name with the Kustomization that
// applies them.
func (c *Component) Key() string { return KeyFor(c.Kind, c.Namespace, c.Name) }

// KeyFor builds a component key.
func KeyFor(kind, namespace, name string) string {
	if kind == KindHelmRelease {
		return KindHelmRelease + "/" + namespace + "/" + name
	}
	return namespace + "/" + name
}

// DepKey is the key of something c dependsOn: dependencies are always of c's
// own kind.
func (c *Component) DepKey(ref Ref) string { return KeyFor(c.Kind, ref.Namespace, ref.Name) }

// IsHelmRelease reports whether c is rendered by helm-controller.
func (c *Component) IsHelmRelease() bool { return c.Kind == KindHelmRelease }

func (c *Component) String() string {
	if c.IsRoot {
		return c.Name
	}
	return c.Key()
}

// BlocksOnHealth reports whether ready(c) waits for applied objects to be healthy.
func (c *Component) BlocksOnHealth() bool { return c.Wait || c.Health }

// Ancestors returns c, its parent, grandparent, ... up to the root.
func (c *Component) Ancestors() []*Component {
	var out []*Component
	for x := c; x != nil; x = x.Parent {
		out = append(out, x)
	}
	return out
}

// Within reports whether c is root or a descendant of root.
func (c *Component) Within(root *Component) bool {
	for x := c; x != nil; x = x.Parent {
		if x == root {
			return true
		}
	}
	return false
}

// Tree is everything rendered from one entrypoint.
type Tree struct {
	RepoRoot   string // absolute
	Entrypoint string
	Root       *Component
	Components []*Component // stable order, root first
	ByKey      map[string]*Component
}

// VarUse is one post-build variable referenced by an object of a component.
type VarUse struct {
	Name       string
	Object     Object
	Defined    bool
	HasDefault bool // ${var:-x} / ${var:=x}: safe even when undefined
}

// Contract is what a component declares it needs in order to run. It ships
// with the component (fluxlint-contract.yaml at its tag), where the people who
// know can keep it true — ideally proven by a test that starts the controller
// with exactly these things present.
type Contract struct {
	Requires struct {
		// CRDs the controller watches and cannot start without.
		CRDs []ContractCRD `json:"crds"`
		// Secrets and ConfigMaps read at runtime, in the component's namespace.
		Secrets    []ContractConfig `json:"secrets"`
		ConfigMaps []ContractConfig `json:"configMaps"`
	} `json:"requires"`
}

type ContractCRD struct {
	Group   string `json:"group"`
	Kind    string `json:"kind"`
	Version string `json:"version"` // optional: must be served
}

type ContractConfig struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"` // optional: defaults to where the component's workloads run
	Keys      []string `json:"keys"`
}
