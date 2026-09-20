package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, content string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), DefaultFile)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestMissingFileGivesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RepoSource.Kind != "GitRepository" || c.RepoSource.Name != "flux-system" || c.RepoSource.Namespace != "flux-system" {
		t.Errorf("repoSource = %+v", c.RepoSource)
	}
	if c.KubeVersion != DefaultKubeVersion || c.Timing.DependencyRequeue.Duration != 30*time.Second || c.Timing.DominantShare != 0.4 {
		t.Errorf("defaults = %q, %v, %v", c.KubeVersion, c.Timing.DependencyRequeue.Duration, c.Timing.DominantShare)
	}
}

func TestFileOverridesDefaults(t *testing.T) {
	c, err := load(t, `
entrypoints: [clusters/production]
kubeVersion: "1.33.2"
repoSource: {name: fleet}
externals:
  namespaces: [tenant-a]
  secrets:
    - {namespace: flux-system, name: api-credentials, keys: [token]}
  runtimeCRDs:
    - {group: infrastructure.cluster.x-k8s.io, providedBy: flux-system/providers}
rules:
  FL-T007: "off"
timing:
  maxBootstrapBound: 30m
  dependencyRequeue: "45s"
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.RepoSource.Name != "fleet" || c.RepoSource.Kind != "GitRepository" {
		t.Errorf("a partial repoSource keeps the other defaults: %+v", c.RepoSource)
	}
	if c.KubeVersion != "1.33.2" || c.Timing.MaxBootstrapBound.Duration != 30*time.Minute || c.Timing.DependencyRequeue.Duration != 45*time.Second {
		t.Errorf("got %q, %v, %v", c.KubeVersion, c.Timing.MaxBootstrapBound.Duration, c.Timing.DependencyRequeue.Duration)
	}
	if len(c.Externals.Secrets) != 1 || c.Externals.Secrets[0].Keys[0] != "token" || c.Externals.RuntimeCRDs[0].ProvidedBy != "flux-system/providers" {
		t.Errorf("externals = %+v", c.Externals)
	}
}

// A misspelt key must not silently turn a setting off.
func TestUnknownKeysAndBadValuesAreErrors(t *testing.T) {
	for name, content := range map[string]string{
		"unknown top-level key": "entrypoint: clusters/production\n",
		"unknown nested key":    "externals:\n  namespace: [a]\n",
		"bad duration":          "timing:\n  maxBootstrapBound: soon\n",
		"not YAML":              "entrypoints: [\n",
	} {
		if _, err := load(t, content); err == nil {
			t.Errorf("%s: no error", name)
		} else if !strings.Contains(err.Error(), DefaultFile) {
			t.Errorf("%s: the error should name the file: %v", name, err)
		}
	}
}
