package contracttest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/contract"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func load(t *testing.T) *contract.Contract {
	t.Helper()
	c, err := contract.Load("../testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func custom(kind, version, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "platform.example.test", Version: version, Kind: kind})
	u.SetNamespace("operator-system")
	u.SetName(name)
	return u
}

// spy is a testing.TB that records failures so a test can assert on them.
type spy struct {
	testing.TB
	errors   []string
	cleanups []func()
}

func (s *spy) Helper()          {}
func (s *spy) Cleanup(f func()) { s.cleanups = append(s.cleanups, f) }
func (s *spy) Errorf(format string, args ...any) {
	s.errors = append(s.errors, fmt.Sprintf(format, args...))
}
func (s *spy) finish() {
	for _, f := range s.cleanups {
		f()
	}
}

// reconcile stands in for a controller: it reads its credentials, the Widget
// it was asked about and, when asked to, a kind the contract does not mention.
func reconcile(ctx context.Context, c client.Client, alsoRead *unstructured.Unstructured) (token string, err error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "operator-system", Name: "api-credentials"}, &secret); err != nil {
		return "", err
	}
	widget := custom("Widget", "v1", "w")
	if err := c.Get(ctx, client.ObjectKeyFromObject(widget), widget); err != nil {
		return "", err
	}
	if alsoRead != nil {
		if err := c.Get(ctx, client.ObjectKeyFromObject(alsoRead), alsoRead); err != nil {
			return "", err
		}
	}
	return string(secret.Data["SERVICE_TOKEN"]) + "|" + string(secret.Data["TELEMETRY_TOKEN"]), nil
}

func TestCodeWithinTheContractPasses(t *testing.T) {
	tb := &spy{TB: t}
	env := New(tb, load(t), InNamespace("operator-system"), WithObjects(custom("Widget", "v1", "w")),
		WithValue("Secret", "api-credentials", "SERVICE_TOKEN", "s3cret"))

	token, err := reconcile(context.Background(), env.Client, nil)
	if err != nil {
		t.Fatal(err)
	}
	// the declared key has the value the test gave it; an undeclared key is absent
	if token != "s3cret|" {
		t.Errorf("token = %q", token)
	}
	tb.finish()
	if len(tb.errors) != 0 {
		t.Errorf("unexpected failures: %v", tb.errors)
	}

	// declared objects land where the contract says
	var registry corev1.Secret
	if err := env.Client.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "registry"}, &registry); err != nil {
		t.Errorf("the Secret declared in namespace shared: %v", err)
	}
	var settings corev1.ConfigMap
	if err := env.Client.Get(context.Background(), client.ObjectKey{Namespace: "operator-system", Name: "settings"}, &settings); err != nil || settings.Data["region"] != "contract-test" {
		t.Errorf("settings = %v, %v", settings.Data, err)
	}
}

func TestUndeclaredKindFailsLikeAMissingCRD(t *testing.T) {
	tb := &spy{TB: t}
	env := New(tb, load(t), InNamespace("operator-system"), WithObjects(custom("Widget", "v1", "w")))

	_, err := reconcile(context.Background(), env.Client, custom("Sprocket", "v1", "s"))
	if !meta.IsNoMatchError(err) {
		t.Fatalf("want a no-match error, got %v", err)
	}
	tb.finish()
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], "platform.example.test Sprocket is used, but requires.crds does not declare it") {
		t.Errorf("failures = %v", tb.errors)
	}
}

func TestDeclaredKindAtAnotherVersion(t *testing.T) {
	tb := &spy{TB: t}
	env := New(tb, load(t), InNamespace("operator-system"), WithObjects(custom("Widget", "v1", "w")))

	// Widget is declared at v1; Gadget is declared without a version
	if _, err := reconcile(context.Background(), env.Client, custom("Widget", "v1beta1", "w")); !meta.IsNoMatchError(err) {
		t.Fatalf("want a no-match error, got %v", err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "platform.example.test", Version: "v2", Kind: "GadgetList"})
	if err := env.Client.List(context.Background(), list); err != nil {
		t.Errorf("Gadget is declared at any version: %v", err)
	}
	tb.finish()
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], "is used at v1beta1, but the contract declares v1") {
		t.Errorf("failures = %v", tb.errors)
	}
}

func TestBuiltinGroups(t *testing.T) {
	for group, want := range map[string]bool{
		"": true, "apps": true, "batch": true, "rbac.authorization.k8s.io": true, "networking.k8s.io": true,
		"cluster.x-k8s.io": false, "platform.example.test": false, "cert-manager.io": false,
	} {
		if builtin(group) != want {
			t.Errorf("builtin(%q) = %v", group, !want)
		}
	}
}
