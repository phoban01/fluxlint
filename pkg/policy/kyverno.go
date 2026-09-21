package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Kyverno's engine cannot be linked: it builds against forks of Kubernetes
// libraries that cannot coexist with the ones Flux and Helm need. Its CLI is
// that engine as a single binary, so the policies are evaluated by it, at the
// version the user installs, which can be the version their clusters run.

// ErrNoKyverno is returned when the kyverno CLI is not installed.
var ErrNoKyverno = errors.New("the kyverno CLI is not on PATH")

// KyvernoResult is one rule evaluated against one object.
type KyvernoResult struct {
	Policy, Rule string
	// Object identifies the resource as apiVersion, kind, namespace, name.
	APIVersion, Kind, Namespace, Name string
	// Result is fail, error or skip; passes are dropped.
	Result  string
	Message string
	// Enforced says whether the policy rejects (Enforce / Deny) or only
	// records (Audit) a failure for this object.
	Enforced bool
}

// IsKyvernoPolicy reports whether o is a Kyverno policy with rules that
// validate, and therefore can reject an apply.
func IsKyvernoPolicy(o map[string]any) bool {
	apiVersion, _ := o["apiVersion"].(string)
	kind, _ := o["kind"].(string)
	switch {
	case strings.HasPrefix(apiVersion, "kyverno.io/") && (kind == "ClusterPolicy" || kind == "Policy"):
		for _, r := range list(o, "spec", "rules") {
			if m, ok := r.(map[string]any); ok && (m["validate"] != nil || m["verifyImages"] != nil) {
				return true
			}
		}
		return false
	case strings.HasPrefix(apiVersion, "policies.kyverno.io/"):
		return kind == "ValidatingPolicy" || kind == "NamespacedValidatingPolicy" || kind == "ImageValidatingPolicy"
	}
	return false
}

func isKyvernoException(o map[string]any) bool {
	apiVersion, _ := o["apiVersion"].(string)
	return strings.Contains(apiVersion, "kyverno.io/") && o["kind"] == "PolicyException"
}

func get(o any, path ...string) any {
	for _, p := range path {
		m, ok := o.(map[string]any)
		if !ok {
			return nil
		}
		o = m[p]
	}
	return o
}

func list(o any, path ...string) []any { l, _ := get(o, path...).([]any); return l }

func str(o any, path ...string) string { s, _ := get(o, path...).(string); return s }

// enforced reports whether a failure of rule in namespace rejects the apply.
func enforced(policy map[string]any, rule, namespace string) bool {
	if strings.HasPrefix(str(policy, "apiVersion"), "policies.kyverno.io/") {
		for _, a := range list(policy, "spec", "validationActions") {
			if a == "Deny" {
				return true
			}
		}
		return false
	}
	action := str(policy, "spec", "validationFailureAction")
	var overrides []any
	for _, r := range list(policy, "spec", "rules") {
		if str(r, "name") != rule {
			continue
		}
		if a := str(r, "validate", "failureAction"); a != "" {
			action = a
		}
		overrides = list(r, "validate", "failureActionOverrides")
	}
	overrides = append(overrides, list(policy, "spec", "validationFailureActionOverrides")...)
	for _, ov := range overrides {
		for _, ns := range list(ov, "namespaces") {
			if ok, _ := filepath.Match(fmt.Sprint(ns), namespace); ok && str(ov, "action") != "" {
				action = str(ov, "action")
			}
		}
	}
	return strings.EqualFold(action, "Enforce")
}

// Kyverno evaluates policies against resources with the kyverno CLI named by
// command ("kyverno" when empty).
func Kyverno(ctx context.Context, command string, policies, resources []map[string]any) ([]KyvernoResult, error) {
	if command == "" {
		command = "kyverno"
	}
	bin, err := exec.LookPath(command)
	if err != nil {
		return nil, ErrNoKyverno
	}
	dir, err := os.MkdirTemp("", "fluxlint-kyverno-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	write := func(name string, objs []map[string]any) (string, error) {
		var buf bytes.Buffer
		for _, o := range objs {
			b, err := yaml.Marshal(o)
			if err != nil {
				return "", err
			}
			buf.WriteString("---\n")
			buf.Write(b)
		}
		path := filepath.Join(dir, name)
		return path, os.WriteFile(path, buf.Bytes(), 0o600)
	}
	exceptions := false
	for _, o := range resources {
		exceptions = exceptions || isKyvernoException(o)
	}
	polFile, err := write("policies.yaml", policies)
	if err != nil {
		return nil, err
	}
	resFile, err := write("resources.yaml", resources)
	if err != nil {
		return nil, err
	}
	args := []string{"apply", polFile, "--resource", resFile, "--policy-report", "--output-format", "json", "--remove-color"}
	if exceptions {
		args = append(args, "--exceptions-with-resources")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	// the CLI must not find a kubeconfig and start asking a cluster
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(dir, "none"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// the report follows any notices the CLI prints first
	out := stdout.Bytes()
	start := bytes.Index(out, []byte(`{"kind"`))
	if start < 0 {
		start = bytes.IndexByte(out, '{')
	}
	if start < 0 {
		if runErr != nil {
			return nil, fmt.Errorf("kyverno apply: %v: %s", runErr, firstLine(stderr.String()))
		}
		return nil, nil // nothing matched any policy
	}
	var report struct {
		Results []struct {
			Policy    string `json:"policy"`
			Rule      string `json:"rule"`
			Result    string `json:"result"`
			Message   string `json:"message"`
			Resources []struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				Namespace  string `json:"namespace"`
				Name       string `json:"name"`
			} `json:"resources"`
		} `json:"results"`
	}
	if err := json.NewDecoder(bytes.NewReader(out[start:])).Decode(&report); err != nil {
		return nil, fmt.Errorf("kyverno apply printed a report that does not parse: %w", err)
	}
	byName := map[string]map[string]any{}
	for _, p := range policies {
		byName[str(p, "metadata", "name")] = p
	}
	var results []KyvernoResult
	for _, r := range report.Results {
		if r.Result == "pass" {
			continue
		}
		for _, res := range r.Resources {
			results = append(results, KyvernoResult{Policy: r.Policy, Rule: r.Rule, Result: r.Result, Message: r.Message,
				APIVersion: res.APIVersion, Kind: res.Kind, Namespace: res.Namespace, Name: res.Name,
				Enforced: enforced(byName[r.Policy], r.Rule, res.Namespace)})
		}
	}
	return results, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
