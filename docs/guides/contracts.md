# Component contracts

fluxlint reads what a controller needs off the manifests it renders: the CRDs of the
custom resources you apply, the Secrets its pods mount, the API groups its RBAC names.
What it cannot see is the controller's *code* — which of those APIs it refuses to start
without, which environment variables it reads and dies on.

A contract closes that gap. It is a small file that ships **with the component**, at the
tag you pin, written by the people who know:

```yaml
# fluxlint-contract.yaml — next to the kustomization a Flux Kustomization renders,
# at the root of the repository, or beside Chart.yaml in a Helm chart (subcharts'
# contracts are merged into the release's)
requires:
  crds:
    - {group: example.io, kind: Widget, version: v1}     # version optional
    - {group: cert-manager.io, kind: Certificate}
  secrets:
    - name: operator-credentials        # in the namespace the component's workloads run in
      keys: [SERVICE_URL, SERVICE_TOKEN]
    - {name: registry-ca, namespace: kube-system}
  configMaps:
    - {name: operator-settings, keys: [region]}
```

For every Kustomization that renders from that source, fluxlint then reports `FL-C001` when

- a required CRD is installed by nothing in the repository (error), or served only in
  other versions (error);
- a required CRD is installed, but nothing orders the installer before the component
  (warning: the controller crash-loops until it appears);
- a required Secret or ConfigMap has no producer, or its producer — a manifest, an
  `ExternalSecret`, a `ClusterExternalSecret` selecting the namespace, a cert-manager
  `Certificate` — does not define a required key (error).

CRDs that only exist at runtime are declared in the consuming repository's
`externals.runtimeCRDs`, and count as installed by the component named there.

## Keeping a contract true

A contract is only worth having if it cannot drift from the code. The component's own
tests are the place to hold it. fluxlint publishes a small Go module for this:

```bash
go get github.com/phoban01/fluxlint/contract
```

It is a separate module with few dependencies. Importing it does not pull fluxlint's
Kubernetes and Helm versions into your operator.

### With a fake client

`contracttest` builds a controller-runtime fake client that contains exactly what the
contract declares. It needs no API server and no test binaries, so it runs anywhere
`go test` runs.

```go
func TestReconcileWithinContract(t *testing.T) {
	c, err := contract.Load("../fluxlint-contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := contracttest.New(t, c,
		contracttest.WithScheme(scheme),                 // your API types
		contracttest.InNamespace("operator-system"),
		contracttest.WithObjects(&platformv1.Widget{ /* the object to reconcile */ }),
	)

	r := &WidgetReconciler{Client: env.Client}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
}
```

In this cluster:

* every Secret and ConfigMap the contract names exists, with the keys it names and no
  others. Code that reads a key the contract leaves out gets nothing, and fails the way
  it would in production.
* a custom resource kind the contract does not name is missing. The call returns the
  "no matches for kind" error a cluster gives when nobody installed the CRD, and the
  test fails when it ends:

```
outside the contract: platform.example.test Sprocket is used, but requires.crds does not declare it
```

* a kind declared at `v1` and used at `v1beta1` fails the same way.

Someone who adds a new kind or a new Secret key to the reconciler must add it to the
contract to get the test green. From then on, every repository that deploys the new tag
learns about the requirement from fluxlint, in its merge request, before anything is
applied.

### With envtest

A fake client sees the calls a reconciler makes. It does not see the watches a manager
sets up when it starts, and a watch on a missing CRD is the most common reason a
controller crash-loops. [envtest](https://book.kubebuilder.io/reference/envtest) covers
that: a real API server with only the declared CRDs installed.

```go
func TestManagerStartsWithinContract(t *testing.T) {
	c, err := contract.Load("../fluxlint-contract.yaml")
	if err != nil {
		t.Fatal(err)
	}

	// an API server with the CRDs the contract names, and no others
	env := &envtest.Environment{CRDDirectoryPaths: crdDirsFor(c.Requires.CRDs)}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Stop()

	// the real manager must start and its caches must sync
	mgr := newManager(t, cfg) // the constructor main() uses
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Error(err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the manager did not start with only what the contract requires")
	}
}
```

Use the fake client for every reconciler test, since it costs nothing. Add one envtest
test for the manager if your CI can run it.

## Related checks that need no contract

- `FL-R007 module-skew`: both the component and whatever installs its CRDs are Go
  modules rendered from Git. If the component's `go.mod` requires a newer version of
  the API module than the tag pinned for the CRDs, fluxlint says so.
- RBAC as a hint: API groups named in the RBAC bound to a workload's ServiceAccount count
  as a reason for a `dependsOn` on whatever installs them, so such dependencies are not
  reported as unjustified. RBAC also lists optional integrations, so it never *demands*
  an ordering — that is what the contract is for.
