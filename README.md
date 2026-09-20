# fluxlint

Static convergence and timing analysis for [Flux](https://fluxcd.io) repositories.

fluxlint renders a Flux repository the way kustomize-controller would, links every
component's imports (namespaces, CRDs, sources, substitution variables) against what
other components export, and analyses the resulting dependency graph — without a
cluster, in well under a second for a typical repository.

- **Will it converge?** Bootstrap deadlocks, missing namespaces and sources,
  dual ownership, undefined substitution variables.
- **Will it run?** Pods that reference a Secret, ConfigMap key, ServiceAccount or
  pull secret nothing creates (ExternalSecrets, ClusterExternalSecret namespace
  selectors and cert-manager Certificates count as producers); fail-closed webhooks with
  no backends; JSON patches that address `env` or `args` by position.
- **Will the API server accept it?** Custom resources validated against the CRDs your
  charts and repositories actually install (served versions, schema after defaulting),
  and pod templates evaluated against their namespace's Pod Security level with the API
  server's own checks.
- **How long can it take?** Max-plus critical-path analysis over `dependsOn`,
  `wait`, `timeout` and `retryInterval`: where the time goes, which dependencies are
  not justified by anything rendered, where a failed apply stalls for a full interval.

Status: **pre-alpha (M3)**. See [docs/DESIGN.md](docs/DESIGN.md) for the model and
roadmap.

## Install

```bash
go install github.com/phoban01/fluxlint/cmd/fluxlint@latest
```

## Use

An *entrypoint* is the directory a cluster's bootstrap Kustomization points at — the
`--path` you gave `flux bootstrap`.

```bash
fluxlint check clusters/production clusters/staging
fluxlint check -v                  # entrypoints from .fluxlint.yaml, include suggestions
fluxlint check --format json
fluxlint rules
fluxlint explain FL-G002
```

Exit code `1` when there are errors (`--fail-on warning` to be stricter), `2` for usage
or I/O problems.

```
clusters/production: 12 components (3 not rendered), 214 objects
  error   FL-G002 bootstrap-deadlock  cannot converge from an empty cluster: cycle between flux-system/configs, flux-system/controllers
            ready(flux-system/controllers) -> start(flux-system/configs)  (dependsOn)
            start(flux-system/configs) -> ready(flux-system/controllers)  (source HelmRepository/flux-system/podinfo needed by HelmRelease/flux-system/podinfo)
  warning FL-T007 retry-cliff  flux-system/apps: no retryInterval: a failed apply is not retried for 30m0s (interval)
  info    FL-T001 critical-path  worst-case bootstrap bound is 12m30s
```

## Configure

`.fluxlint.yaml` in the repository root (all fields optional):

```yaml
entrypoints:
  - clusters/production

# The GitRepository that represents this repository (default shown).
repoSource: {kind: GitRepository, name: flux-system, namespace: flux-system}

kubeVersion: "1.32.0"                   # what charts are rendered for

# Things that exist in the cluster but are not produced by anything in Git.
externals:
  namespaces: [tenant-a]
  crdGroups: [example.internal]         # installed by something fluxlint cannot reach
  secrets:                              # created out of band; keys optional but enforced if given
    - {namespace: flux-system, name: api-credentials, keys: [token]}
  substitutions:
    - kind: ConfigMap
      name: cluster-info
      variables: [cluster_name, region]

rules:
  FL-T007: "off"                        # error | warning | info | off

timing:
  maxBootstrapBound: 30m                # fail when the worst-case bound regresses past this
  dependencyRequeue: 30s                # controllers' --requeue-dependency
```

## What is rendered

In-process kustomize builds, Flux's generated `kustomization.yaml` for directories
without one, Kustomization-level `patches`, `images`, `components`, `targetNamespace`,
`namePrefix`/`nameSuffix`, and `postBuild` substitution using Flux's own `envsubst`
package (including `substitute: disabled`).

Kustomizations that read from **another `GitRepository` or an `OCIRepository`** are
rendered too. The source is fetched once at the ref the manifests pin and kept in a
cache (`~/.cache/fluxlint`, `--cache-dir`, or `sources.cacheDir`); later runs are
offline and fast. Credentials are whatever `git` and your Docker config already have.

| Flag | Behaviour |
| --- | --- |
| *(default)* | use the cache, fetch what is missing |
| `--offline` | never touch the network; a miss is reported as `FL-X001` |
| `--refresh` | also re-resolve floating refs (branches, semver ranges, `latest`) |

Floating refs are reported (`FL-X002`): what Flux applies can then change without a
commit to your repository. In CI, where sibling repositories are already checked out,
map a source to a directory instead of fetching it:

```yaml
sources:
  overrides:
    - {kind: GitRepository, name: my-operator, path: ../my-operator}
```

**HelmReleases** are rendered with the Helm SDK (`helm template --include-crds`, in
process) using the release's real `values` and `valuesFrom`, its release name and
target namespace, and the chart version the manifests pin — from HTTP(S) and OCI
`HelmRepository`s, `OCIRepository` chart refs, and charts inside a `GitRepository`.
Each release is a node in the graph with helm-controller's timeout and install
retries, so CRDs that only a chart installs, `dependsOn` between releases, and a
`wait: true` Kustomization that gives up before its releases do are all visible. A
chart that cannot render with your values is an error (`FL-G008`).

Set `kubeVersion` in `.fluxlint.yaml` to what your clusters run: charts gate on it.

Not yet: `spec.postRenderers`, `valuesFrom.targetPath`, Helm hooks, chart
dependencies of Git-hosted charts, and `Bucket` sources. Where a spec uses something
that is not modelled, fluxlint says so (`FL-X003`) instead of guessing, and components
that cannot be rendered lower the confidence of rules that depend on them.

## Runtime-installed CRDs

Some CRDs exist only once a controller is running — cluster-api-operator installs a
provider's CRDs when it reconciles an `InfrastructureProvider`. Nothing in Git renders
them, but their consumers must still be ordered after whatever triggers the install:

```yaml
externals:
  runtimeCRDs:
    - group: infrastructure.cluster.x-k8s.io
      providedBy: flux-system/infra-capi-providers   # or HelmRelease/<ns>/<name>
```

Unlike `crdGroups`, this keeps the ordering check: a consumer with no path from the
provider is reported as `FL-T006`.

## Development

```bash
make test   # hermetic: synthetic fixtures, local git repos, in-process registry and chart server
make e2e    # the built binary against e2e/testdata/platform
```

`e2e/testdata/platform` is a single-cluster repository laid out the way platform teams
usually do it — `clusters/<env>`, layered `infrastructure/` and `apps/`, a shared
variables ConfigMap, blue/green nested Kustomizations — built from real pinned upstreams:
cert-manager, kyverno, cluster-api-operator with the AWS provider, and podinfo both from
its Git repository and as an OCI chart. The baseline must be clean and analyse in under
10s offline; every other test seeds one defect (a HelmRepository behind a `dependsOn`,
a namespace created by the consumer's own child, a chart-installed CRD with no ordering,
a `wait: true` parent that times out before its releases, a version bump to a tag that
does not exist, values a chart's schema rejects, …) and asserts that fluxlint names it.

## Licence

Apache-2.0
