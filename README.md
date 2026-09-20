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
  charts and repositories actually install (served versions, schema after defaulting, the CRD author's CEL rules), built-in
  objects decoded strictly into their Kubernetes types (misspelt fields, wrong types,
  an unquoted `1.31` in a label),
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
fluxlint check --format json            # also: gitlab, sarif, github
fluxlint graph --format mermaid clusters/production
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

## Merge requests: `--base`

```bash
fluxlint check --base origin/main
```

renders both commits (the base via `git archive`, never touching your working tree) and

- **fails only on findings the change introduces** — existing debt is counted but does
  not block, so fluxlint can be made a required check on day one;
- reports what reconciling the change will do to a cluster running the base:
  objects Flux will **prune** (a warning when namespaces, CRDs, PVCs, StatefulSets or
  Secrets are among them), objects **orphaned** because their Kustomization has
  `prune: false`, updates to **immutable fields** that the API server will reject
  (workload selectors, `volumeClaimTemplates`, Job templates, `roleRef` …) unless the
  Kustomization sets `force: true`, and objects that **move between Kustomizations**.

This sees through charts and external repositories: a chart upgrade that changes a
Deployment's selector is reported against the HelmRelease whose version you bumped.

## In CI

Findings carry the repository file and line of the manifest to fix. When the offending
object lives inside a chart or an external repository, that is the HelmRelease or
Kustomization which pulls it in.

```yaml
# GitLab: inline in the merge request widget
fluxlint:
  script:
    - fluxlint check --base origin/$CI_MERGE_REQUEST_TARGET_BRANCH_NAME --format gitlab --output gl-code-quality-report.json
  artifacts:
    when: always
    reports:
      codequality: gl-code-quality-report.json
  cache:
    key: fluxlint-sources
    paths: [.fluxlint-cache]          # with sources.cacheDir: .fluxlint-cache
```

```yaml
# GitHub Actions: inline annotations, or SARIF for code scanning
- run: fluxlint check --format github
- run: fluxlint check --format sarif --output fluxlint.sarif
```

With `--output` the machine-readable report goes to the file and the readable one to the
job log. Code Quality fingerprints ignore line numbers, so GitLab can tell new findings
from existing ones.

## Configure

`.fluxlint.yaml` in the repository root (all fields optional):

```yaml
entrypoints:
  - clusters/production

# The GitRepository that represents this repository (default shown).
repoSource: {kind: GitRepository, name: flux-system, namespace: flux-system}

kubeVersion: "1.35.0"                   # what charts are rendered for

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
package (including `substitute: disabled`). The generated file list honours
source-controller's default exclusions, `.sourceignore` files and the source's
`spec.ignore`, so a `values.yaml` you keep out of the artifact is not mistaken for a
broken manifest.

Kustomizations that read from **another `GitRepository` or an `OCIRepository`** are
rendered too. The source is fetched once at the ref the manifests pin and kept in a
cache (`~/.cache/fluxlint`, `--cache-dir`, or `sources.cacheDir`); later runs are
offline and fast. Credentials are whatever you already have: `git` credential helpers
for Git, your Docker config for OCI, and `$NETRC` / `~/.netrc` for HTTP Helm repositories
(in GitLab CI: `machine gitlab.example.com login gitlab-ci-token password $CI_JOB_TOKEN`).

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

`spec.postRenderers` (kustomize patches and images) are applied, and install/upgrade
hooks are included: a pre-install Job is a real pod with real needs. Test and delete hooks
are left out.

`valuesFrom` follows helm-controller, including `targetPath` with its `--set` /
quoted `--set-string` semantics. Charts that live in a `GitRepository` get the
dependencies in their `Chart.yaml` loaded (`file://`) or fetched (HTTP and OCI
repositories), as source-controller does before packaging them.

Not yet: `Bucket` sources. Where a spec uses something
that is not modelled, fluxlint says so (`FL-X003`) instead of guessing, and components
that cannot be rendered lower the confidence of rules that depend on them.

## Your own invariants

Conventions that only make sense in your repository — blue/green rules, naming,
required labels — are CEL expressions in `.fluxlint.yaml`, evaluated for every rendered
object they match. In scope: `object`, `component`, and `vars`: the resolved post-build
variables that apply to the object (for a nested Kustomization, those of the ancestor
that substituted its spec).

```yaml
assertions:
  - name: live colour serves traffic
    mustMatch: true                       # fail if a rename leaves this guarding nothing
    match: {kind: Deployment, namespace: shop, name: "shop-*"}   # globs; all optional
    expr: '!object.metadata.name.endsWith("-" + vars.shop_live) || object.spec.replicas > 0'
    message: the colour named by shop_live is scaled to zero
    severity: error                       # default
```

This works on objects rendered from charts and external repositories too. An
expression that cannot be evaluated (a typo in a field name) is reported as a failure
rather than silently passing.

## What a controller needs

Controllers rendered from another repository can ship a
[contract](docs/CONTRACTS.md) — the CRDs they will not start without, the Secret keys
they read — and fluxlint holds your repository to it (`FL-C001`). Without one it still
compares the API module version in the controller's `go.mod` with the tag you pin for
the component that installs those CRDs (`FL-R007`).

## Timing with real numbers

The timing analysis reports a worst-case bound. Give it observed reconcile durations and
it also reports an expected time, with the path that determines it:

```bash
kubectl -n flux-system port-forward deploy/kustomize-controller 8080 &
curl -s localhost:8080/metrics | grep gotk_reconcile_duration_seconds > observed.prom
fluxlint check --observed observed.prom
```

A YAML map of component to duration (`flux-system/apps: 42s`) works too.

## The graph

```bash
fluxlint graph --format mermaid clusters/production   # or dot
```

One node per Kustomization and HelmRelease; `dependsOn` solid, parent/child dotted,
imports dashed and labelled, the critical path bold, deadlocks red.

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

`e2e/operators_test.go` covers what public upstreams cannot: an internal platform that is
mocked end to end and needs no network — an API repository and a kubebuilder-style
operator repository (Go modules, RBAC, a contract) served as Git over `file://`, CRDs as
an OCI artifact in an in-process registry, a private Helm repository behind basic auth,
and a cluster repository that adapts the operator the usual way (blue/green nested
Kustomization, version / path / replicas from a shared ConfigMap, inline Secret deleted,
env rewired by index onto a ClusterExternalSecret). Its centrepiece is the routine version
bump after which an untouched positional patch rewires the wrong variables.

## Licence

Apache-2.0
