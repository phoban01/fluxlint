# Component contracts

fluxlint reads what a controller needs off the manifests it renders: the CRDs of the
custom resources you apply, the Secrets its pods mount, the API groups its RBAC names.
What it cannot see is the controller's *code* — which of those APIs it refuses to start
without, which environment variables it reads and dies on.

A contract closes that gap. It is a small file that ships **with the component**, at the
tag you pin, written by the people who know:

```yaml
# fluxlint-contract.yaml — next to the kustomization a Flux Kustomization renders,
# or at the root of the repository
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

A contract is only worth having if it cannot drift. The cheap way to prove it is in the
component's own CI, with [envtest](https://book.kubebuilder.io/reference/envtest) — a
real API server, no nodes, seconds to start:

```go
func TestContract(t *testing.T) {
	contract := loadContract(t, "../fluxlint-contract.yaml")

	// 1. an API server with exactly the CRDs the contract names — no more
	env := &envtest.Environment{CRDDirectoryPaths: crdDirsFor(contract.Requires.CRDs)}
	cfg, err := env.Start()
	require.NoError(t, err)
	defer env.Stop()

	// 2. exactly the environment the contract names
	for _, s := range contract.Requires.Secrets {
		for _, k := range s.Keys {
			t.Setenv(k, "placeholder")
		}
	}

	// 3. the real manager must start and its caches must sync
	mgr := newManager(t, cfg)          // the same constructor main() uses
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { require.NoError(t, mgr.Start(ctx)) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx),
		"manager did not start with only what the contract requires")
}
```

If someone adds a watch on a new CRD or a new `os.Getenv` that the manager dies on, this
test fails until the contract says so — and every repository that deploys the new tag
learns about the requirement from fluxlint, in its merge request, before anything is
applied.

## Related checks that need no contract

- `FL-R007 module-skew`: both the component and whatever installs its CRDs are Go
  modules rendered from Git. If the component's `go.mod` requires a newer version of
  the API module than the tag pinned for the CRDs, fluxlint says so.
- RBAC as a hint: API groups named in the RBAC bound to a workload's ServiceAccount count
  as a reason for a `dependsOn` on whatever installs them, so such dependencies are not
  reported as unjustified. RBAC also lists optional integrations, so it never *demands*
  an ordering — that is what the contract is for.
