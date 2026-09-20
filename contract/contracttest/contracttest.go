// Package contracttest gives a controller's tests a cluster that contains
// exactly what its contract declares, built on controller-runtime's fake
// client. No API server and no test binaries are needed.
//
//	func TestReconcileWithinContract(t *testing.T) {
//		c, err := contract.Load("../fluxlint-contract.yaml")
//		if err != nil {
//			t.Fatal(err)
//		}
//		env := contracttest.New(t, c, contracttest.WithScheme(scheme), contracttest.InNamespace("operator-system"))
//
//		r := &WidgetReconciler{Client: env.Client}
//		if _, err := r.Reconcile(ctx, req); err != nil {
//			t.Fatal(err)
//		}
//	}
//
// The Secrets and ConfigMaps the contract names exist, with the keys it names
// and no others. A custom resource kind the contract does not name is missing,
// the way it is on a cluster where nobody installed its CRD: the call fails with
// a "no matches for kind" error, and the test fails when it ends. Code that
// needs more than the contract says cannot pass.
package contracttest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/phoban01/fluxlint/contract"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Env is a fake cluster shaped by a contract.
type Env struct {
	// Client is what the code under test should use.
	Client client.WithWatch

	mu         sync.Mutex
	violations []string
}

type options struct {
	scheme    *runtime.Scheme
	namespace string
	objects   []client.Object
	values    map[string]string
}

// Option adjusts the environment.
type Option func(*options)

// WithScheme sets the scheme the client uses. It must know the controller's
// own API types. The default knows only the built-in ones.
func WithScheme(s *runtime.Scheme) Option { return func(o *options) { o.scheme = s } }

// InNamespace is where the controller runs: Secrets and ConfigMaps declared
// without a namespace are created here. The default is "default".
func InNamespace(ns string) Option { return func(o *options) { o.namespace = ns } }

// WithObjects adds what the test itself needs, such as the custom resource
// being reconciled.
func WithObjects(objs ...client.Object) Option {
	return func(o *options) { o.objects = append(o.objects, objs...) }
}

// WithValue sets the value of one declared key. kind is "Secret" or
// "ConfigMap". Keys without a value hold the string "contract-test".
func WithValue(kind, name, key, value string) Option {
	return func(o *options) { o.values[kind+"/"+name+"/"+key] = value }
}

// New builds the environment. When the test ends, every step the code took
// outside the contract is reported as a test failure.
func New(t testing.TB, c *contract.Contract, opts ...Option) *Env {
	t.Helper()
	o := &options{namespace: "default", values: map[string]string{}}
	for _, opt := range opts {
		opt(o)
	}
	if o.scheme == nil {
		o.scheme = runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(o.scheme); err != nil {
			t.Fatal(err)
		}
	}

	objects := append([]client.Object(nil), o.objects...)
	value := func(kind, name, key string) string {
		if v, ok := o.values[kind+"/"+name+"/"+key]; ok {
			return v
		}
		return "contract-test"
	}
	for _, s := range c.Requires.Secrets {
		secret := &corev1.Secret{ObjectMeta: meta1(s, o.namespace), Data: map[string][]byte{}}
		for _, k := range s.Keys {
			secret.Data[k] = []byte(value("Secret", s.Name, k))
		}
		objects = append(objects, secret)
	}
	for _, cm := range c.Requires.ConfigMaps {
		configMap := &corev1.ConfigMap{ObjectMeta: meta1(cm, o.namespace), Data: map[string]string{}}
		for _, k := range cm.Keys {
			configMap.Data[k] = value("ConfigMap", cm.Name, k)
		}
		objects = append(objects, configMap)
	}

	env := &Env{}
	check := func(obj runtime.Object) error { return env.check(c, o.scheme, obj) }
	env.Client = fake.NewClientBuilder().
		WithScheme(o.scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := check(obj); err != nil {
					return err
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := check(list); err != nil {
					return err
				}
				return cl.List(ctx, list, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := check(obj); err != nil {
					return err
				}
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := check(obj); err != nil {
					return err
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := check(obj); err != nil {
					return err
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := check(obj); err != nil {
					return err
				}
				return cl.Delete(ctx, obj, opts...)
			},
			Watch: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
				if err := check(list); err != nil {
					return nil, err
				}
				return cl.Watch(ctx, list, opts...)
			},
		}).
		Build()

	// the test's own objects are part of the setup, not of the code under test
	for _, obj := range o.objects {
		if err := env.check(c, o.scheme, obj); err != nil {
			t.Fatalf("WithObjects: %v", err)
		}
	}
	env.violations = nil

	t.Cleanup(func() {
		for _, v := range env.Violations() {
			t.Errorf("outside the contract: %s", v)
		}
	})
	return env
}

// Violations lists what the code under test has used so far that the contract
// does not declare.
func (e *Env) Violations() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.violations...)
}

func meta1(c contract.Config, defaultNamespace string) metav1.ObjectMeta {
	ns := c.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	return metav1.ObjectMeta{Name: c.Name, Namespace: ns}
}

// check fails calls that touch a custom resource kind the contract does not
// declare, the way a cluster without that CRD fails them.
func (e *Env) check(c *contract.Contract, scheme *runtime.Scheme, obj runtime.Object) error {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return err
	}
	if meta.IsListType(obj) {
		gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
	}
	if builtin(gvk.Group) {
		return nil
	}
	for _, crd := range c.Requires.CRDs {
		if crd.Group != gvk.Group || crd.Kind != gvk.Kind {
			continue
		}
		if crd.Version == "" || crd.Version == gvk.Version {
			return nil
		}
		e.record(fmt.Sprintf("%s %s is used at %s, but the contract declares %s", gvk.Group, gvk.Kind, gvk.Version, crd.Version))
		return noMatch(gvk)
	}
	e.record(fmt.Sprintf("%s %s is used, but requires.crds does not declare it", gvk.Group, gvk.Kind))
	return noMatch(gvk)
}

func (e *Env) record(v string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, seen := range e.violations {
		if seen == v {
			return
		}
	}
	e.violations = append(e.violations, v)
}

func noMatch(gvk schema.GroupVersionKind) error {
	return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
}

// builtin reports whether an API group ships with Kubernetes: the core group,
// the unqualified ones (apps, batch, policy …) and *.k8s.io. Groups under
// x-k8s.io are CRDs.
func builtin(group string) bool {
	return !strings.Contains(group, ".") || (strings.HasSuffix(group, ".k8s.io") && !strings.HasSuffix(group, ".x-k8s.io"))
}
