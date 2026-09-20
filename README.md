# fluxlint

Static convergence and timing analysis for [Flux](https://fluxcd.io) repositories.

fluxlint renders a Flux repository the way kustomize-controller would, links every
component's imports (namespaces, CRDs, sources, substitution variables) against what
other components export, and analyses the resulting dependency graph — without a
cluster, in well under a second for a typical repository.

- **Will it converge?** Bootstrap deadlocks, missing namespaces and sources,
  dual ownership, undefined substitution variables.
- **How long can it take?** Max-plus critical-path analysis over `dependsOn`,
  `wait`, `timeout` and `retryInterval`: where the time goes, which dependencies are
  not justified by anything rendered, where a failed apply stalls for a full interval.

Status: **pre-alpha (M2a)**. See [docs/DESIGN.md](docs/DESIGN.md) for the model and
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

# Things that exist in the cluster but are not produced by anything in Git.
externals:
  namespaces: [tenant-a]
  crdGroups: [cert-manager.io]          # installed by a chart fluxlint cannot render yet
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

Not yet: the contents of Helm charts, and `Bucket` sources. Components that cannot be
rendered are reported as such; rules that depend on them lower their confidence
instead of guessing.

## Licence

Apache-2.0
