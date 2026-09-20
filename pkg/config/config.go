// Package config loads .fluxlint.yaml.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// DefaultKubeVersion is used to render charts when kubeVersion is not set.
const DefaultKubeVersion = "1.35.0"

// DefaultFile is looked up in the repository root.
const DefaultFile = ".fluxlint.yaml"

// Config is the user-facing configuration.
type Config struct {
	// Entrypoints are directories (relative to the repo root) that a cluster's
	// bootstrap Kustomization points at, e.g. "clusters/production".
	// An entry is a path, or an object with a path and settings for that
	// cluster alone.
	Entrypoints []Entrypoint `json:"entrypoints"`

	// RepoSource names the GitRepository that represents this repository.
	RepoSource SourceRef `json:"repoSource"`

	// Externals declares things that exist in the cluster but are not produced
	// by anything in Git.
	Externals Externals `json:"externals"`

	// KubeVersion is the Kubernetes version charts are rendered for
	// (.Capabilities.KubeVersion and the chart's kubeVersion constraint).
	// Set it to what your clusters run; the default only exists because
	// Helm's own (v1.20) is rejected by most current charts.
	KubeVersion string `json:"kubeVersion"`

	Sources Sources `json:"sources"`

	// Rules selects which rules run and how severe their findings are.
	Rules Rules `json:"rules"`

	// Assertions are repository-specific invariants, see Assertion.
	Assertions []Assertion `json:"assertions"`

	Timing Timing `json:"timing"`

	Contracts Contracts `json:"contracts"`
}

// Sources configures how external Flux sources are materialised.
type Sources struct {
	// CacheDir defaults to the user cache directory.
	CacheDir string `json:"cacheDir"`
	// Overrides map a source object to a local directory (relative to the
	// repository root), e.g. a sibling checkout in a CI job.
	Overrides []SourceOverride `json:"overrides"`
}

type SourceOverride struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
}

// Assertion is a CEL expression that must hold for every rendered object it
// matches. In scope: object (the rendered object), vars (the owning
// component's resolved post-build variables) and component (its key).
type Assertion struct {
	Name     string `json:"name"`
	Match    Match  `json:"match"`
	Expr     string `json:"expr"`
	Message  string `json:"message"`
	Severity string `json:"severity"` // error (default), warning or info
	// MustMatch fails the assertion when nothing matches, so that a rename
	// cannot quietly disable it.
	MustMatch bool `json:"mustMatch"`
}

// Match selects objects; every field is an optional glob.
type Match struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Component string `json:"component"`
}

type SourceRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type Externals struct {
	Namespaces []string `json:"namespaces"`
	// ConfigMaps and Secrets read by postBuild.substituteFrom, with the
	// variables they are known to provide.
	Substitutions []ExternalSubstitution `json:"substitutions"`
	// CRDGroups are API groups whose CRDs are installed out of band or by a
	// component fluxlint cannot render yet.
	CRDGroups []string `json:"crdGroups"`
	// Secrets created out of band (bootstrap credentials, secrets written by
	// a controller at runtime). Keys is optional; when given, key references
	// are checked against it.
	Secrets []ExternalSecretRef `json:"secrets"`
	// RuntimeCRDs are API groups whose CRDs appear only once a component is
	// running (an operator that installs providers, a controller that
	// registers its own types). Unlike CRDGroups they keep their ordering:
	// consumers must still come after ProvidedBy is Ready.
	RuntimeCRDs []RuntimeCRD `json:"runtimeCRDs"`
}

type ExternalSecretRef struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Keys      []string `json:"keys"`
}

type RuntimeCRD struct {
	Group string `json:"group"`
	// ProvidedBy is a component key: "namespace/name" for a Kustomization,
	// "HelmRelease/namespace/name" for a HelmRelease.
	ProvidedBy string `json:"providedBy"`
}

type ExternalSubstitution struct {
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Variables []string `json:"variables"`
}

type Timing struct {
	// MaxBootstrapBound fails the run when the worst-case bound exceeds it.
	MaxBootstrapBound Duration `json:"maxBootstrapBound"`
	// DependencyRequeue is the controllers' --requeue-dependency (default 30s).
	DependencyRequeue Duration `json:"dependencyRequeue"`
	// Observed are mean reconcile durations per component key, supplied with
	// --observed. They turn the worst-case bound into an estimate as well.
	Observed map[string]time.Duration `json:"-"`
	// DominantShare is the fraction of the bound above which a single
	// component is reported as dominant (default 0.4).
	DominantShare float64 `json:"dominantShare"`
}

// Duration parses Go duration strings from YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Default returns the configuration used when no file exists.
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.RepoSource.Kind == "" {
		c.RepoSource.Kind = "GitRepository"
	}
	if c.RepoSource.Name == "" {
		c.RepoSource.Name = "flux-system"
	}
	if c.RepoSource.Namespace == "" {
		c.RepoSource.Namespace = "flux-system"
	}
	if c.KubeVersion == "" {
		c.KubeVersion = DefaultKubeVersion
	}
	if c.Timing.DependencyRequeue.Duration == 0 {
		c.Timing.DependencyRequeue.Duration = 30 * time.Second
	}
	if c.Timing.DominantShare == 0 {
		c.Timing.DominantShare = 0.4
	}
}

// Load reads path; a missing file yields the defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.UnmarshalStrict(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults()
	return c, nil
}

// Rules selects rules the way golangci-lint selects linters: start from all of
// them or none, then enable or disable individual ones. A rule is named by its
// ID ("FL-T007") or its name ("retry-cliff"); a family name ("timing") stands
// for every rule in it.
type Rules struct {
	// Default is "all" (the default) or "none".
	Default string `json:"default"`
	// Enable adds rules. It matters when Default is "none".
	Enable []string `json:"enable"`
	// Disable removes rules. It matters when Default is "all".
	Disable []string `json:"disable"`
	// Severity overrides a rule's severity: "error", "warning" or "info".
	Severity map[string]string `json:"severity"`

	// Off and Level are what the above resolve to, keyed by rule ID. The lint
	// package fills them in, because it owns the catalogue.
	Off   map[string]bool   `json:"-"`
	Level map[string]string `json:"-"`
}

// Contracts configures where contracts are looked for beyond the sources
// themselves.
type Contracts struct {
	// Images are patterns ("registry.example.com/platform/*") for container
	// images to ask for an attached contract. Only images that match are looked
	// up, so fluxlint asks your registry about your images and nobody else's.
	Images []string `json:"images"`
}
