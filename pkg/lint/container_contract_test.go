package lint_test

import (
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/source"
)

// containerRepo is an operator whose contract speaks about its container,
// deployed by the given pod spec.
func containerRepo(t *testing.T, contract, podSpec string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "go.mod", "module example.test/operator\n\ngo 1.22\n")
	write(t, dir, "fluxlint-contract.yaml", contract)
	write(t, dir, "config/manager.yaml", deployment("manager", "operator-system", 1, podSpec))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "operator")
	git(t, dir, "tag", "v2.0.0")
	return dir
}

func TestContainerContract(t *testing.T) {
	const byName = `requires:
  containers:
    - name: manager
      env: [WATCH_NAMESPACE]
      args:
        required: [--namespace]
        accepted: [--namespace, --leader-elect]
`
	const byImage = `requires:
  containers:
    - image: example.test/operator
      env: [WATCH_NAMESPACE]
`
	const unselected = `requires:
  containers:
    - env: [WATCH_NAMESPACE]
`
	pod := func(container string) string {
		return "containers:\n  - name: manager\n    image: example.test/operator:1\n" + indent(container, 4)
	}
	const envSet = "env:\n  - {name: WATCH_NAMESPACE, value: tenants}\n"
	settings := func(keys string) string {
		return "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: operator-system}\ndata:\n" + keys
	}

	api := apiRepo(t)
	for _, tc := range []struct {
		name, contract, pod, extra string
		want                       []string // substrings of FL-C001 messages, or empty for none
		note                       string   // substring of an FL-X003 note
	}{
		{name: "nothing passed", contract: byName, pod: pod("args: [--leader-elect]\n"),
			want: []string{"requires environment variable WATCH_NAMESPACE in container manager of", "requires flag --namespace in container manager of"}},
		{name: "met", contract: byName, pod: pod(envSet + "args: [--namespace=tenants, --leader-elect]\n")},
		{name: "single dash and separate value", contract: byName, pod: pod(envSet + "args: [-namespace, tenants]\n")},
		{name: "empty literal is unset", contract: byName, pod: pod("env:\n  - {name: WATCH_NAMESPACE, value: \"\"}\nargs: [--namespace=x]\n"),
			want: []string{"requires environment variable WATCH_NAMESPACE"}},
		{name: "set by valueFrom", contract: byName, pod: pod("env:\n  - {name: WATCH_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}\nargs: [--namespace=x]\n")},
		{name: "envFrom provides the key", contract: byName, pod: pod("envFrom:\n  - configMapRef: {name: settings}\nargs: [--namespace=x]\n"),
			extra: settings("  WATCH_NAMESPACE: tenants\n")},
		{name: "envFrom with prefix", contract: byName, pod: pod("envFrom:\n  - {prefix: WATCH_, configMapRef: {name: settings}}\nargs: [--namespace=x]\n"),
			extra: settings("  NAMESPACE: tenants\n")},
		{name: "envFrom lacks the key", contract: byName, pod: pod("envFrom:\n  - configMapRef: {name: settings}\nargs: [--namespace=x]\n"),
			extra: settings("  OTHER: x\n"), want: []string{"requires environment variable WATCH_NAMESPACE"}},
		{name: "unknown flag", contract: byName, pod: pod(envSet + "args: [--namespace=x, --old-flag=1]\n"),
			want: []string{"passes --old-flag to container manager of"}},
		{name: "image command line is unseen", contract: byName, pod: pod(envSet)},
		{name: "shell script is unseen", contract: byName, pod: pod(envSet + "command: [/bin/sh, -c, \"exec /manager --namespace=x\"]\n")},
		{name: "shell behind env is unseen", contract: byName, pod: pod(envSet + "command: [/usr/bin/env]\nargs: [sh, -c, \"exec /manager --namespace=x\"]\n")},
		{name: "init wrapper's -- does not stop the scan", contract: byName, pod: pod(envSet + "command: [/tini, \"--\"]\nargs: [/manager, --namespace=x]\n")},
		{name: "unknown flag after an init wrapper", contract: byName, pod: pod(envSet + "command: [/tini, \"--\"]\nargs: [/manager, --namespace=x, --old-flag]\n"),
			want: []string{"passes --old-flag to container manager of"}},
		{name: "unquoted number is set, not unset", contract: byName, pod: pod("env:\n  - {name: WATCH_NAMESPACE, value: 1234}\nargs: [--namespace=x]\n")},
		{name: "selected by image", contract: byImage, pod: pod("args: [--namespace=x]\n"),
			want: []string{"requires environment variable WATCH_NAMESPACE in container manager of"}},
		{name: "other container", contract: byName, pod: "containers:\n  - name: proxy\n    image: example.test/proxy:1\n    args: [--old-flag]\n"},
		{name: "git contract names no container", contract: unselected, pod: pod("args: [--namespace=x]\n"),
			note: "requires.containers[0] names no container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := containerRepo(t, tc.contract, tc.pod)
			r := analyseExternal(t, controllerCluster(t, api, repo, "v1.1.0", "", tc.extra), source.Options{CacheDir: t.TempDir()})
			got := messages(find(r, "FL-C001"))
			if len(tc.want) == 0 && got != "" {
				t.Errorf("unexpected FL-C001:\n%s", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("FL-C001 lacks %q:\n%s", want, messages(r.Findings))
				}
			}
			if tc.note != "" && !strings.Contains(messages(find(r, "FL-X003")), tc.note) {
				t.Errorf("no render note containing %q:\n%s", tc.note, messages(r.Findings))
			}
			if len(find(r, "FL-C001")) != len(tc.want) {
				t.Errorf("want %d FL-C001 findings:\n%s", len(tc.want), got)
			}
		})
	}
}
