package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Entrypoint is one cluster. Clusters in one repository differ: they run
// different Kubernetes versions, and what exists out of band in one does not
// in another. Everything here applies to this entrypoint only, on top of the
// top-level settings.
type Entrypoint struct {
	Path string `json:"path"`
	// KubeVersion and RepoSource replace the top-level values.
	KubeVersion string     `json:"kubeVersion,omitempty"`
	RepoSource  *SourceRef `json:"repoSource,omitempty"`
	// Externals are added to the top-level externals.
	Externals Externals `json:"externals,omitempty"`
	// Timing fields that are set replace the top-level ones.
	Timing Timing `json:"timing,omitempty"`
}

// UnmarshalJSON accepts a bare path or an object.
func (e *Entrypoint) UnmarshalJSON(b []byte) error {
	var path string
	if err := json.Unmarshal(b, &path); err == nil {
		*e = Entrypoint{Path: path}
		return nil
	}
	type plain Entrypoint
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // the outer strict decode does not reach into here
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("entrypoints: %w", err)
	}
	if p.Path == "" {
		return fmt.Errorf("entrypoints: an entry needs a path")
	}
	*e = Entrypoint(p)
	return nil
}

// Paths lists the configured entrypoints.
func (c *Config) Paths() []string {
	out := make([]string, len(c.Entrypoints))
	for i, e := range c.Entrypoints {
		out[i] = e.Path
	}
	return out
}

// For returns the configuration that applies to one entrypoint. A path that is
// not configured, such as one given on the command line, gets the top-level
// settings unchanged.
func (c *Config) For(path string) *Config {
	for _, e := range c.Entrypoints {
		if filepath.Clean(e.Path) != filepath.Clean(path) {
			continue
		}
		out := *c
		if e.KubeVersion != "" {
			out.KubeVersion = e.KubeVersion
		}
		if e.RepoSource != nil {
			out.RepoSource = *e.RepoSource
			out.applyDefaults()
		}
		x, y := c.Externals, e.Externals
		out.Externals = Externals{
			Namespaces:    join(x.Namespaces, y.Namespaces),
			Substitutions: join(x.Substitutions, y.Substitutions),
			CRDGroups:     join(x.CRDGroups, y.CRDGroups),
			Secrets:       join(x.Secrets, y.Secrets),
			RuntimeCRDs:   join(x.RuntimeCRDs, y.RuntimeCRDs),
		}
		if e.Timing.MaxBootstrapBound.Duration != 0 {
			out.Timing.MaxBootstrapBound = e.Timing.MaxBootstrapBound
		}
		if e.Timing.DependencyRequeue.Duration != 0 {
			out.Timing.DependencyRequeue = e.Timing.DependencyRequeue
		}
		if e.Timing.DominantShare != 0 {
			out.Timing.DominantShare = e.Timing.DominantShare
		}
		return &out
	}
	return c
}

// join concatenates without sharing a backing array with either input.
func join[T any](a, b []T) []T {
	return append(append([]T(nil), a...), b...)
}
