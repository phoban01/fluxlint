# fluxlint

[![ci](https://github.com/phoban01/fluxlint/actions/workflows/ci.yml/badge.svg)](https://github.com/phoban01/fluxlint/actions/workflows/ci.yml)
[![license](https://img.shields.io/github/license/phoban01/fluxlint.svg)](LICENSE)
[![docs](https://img.shields.io/badge/docs-phoban01.github.io%2Ffluxlint-blue)](https://phoban01.github.io/fluxlint/)

fluxlint is a static analyser for [Flux](https://fluxcd.io) repositories. It finds
changes that would stop a cluster from reconciling, before they merge and without a
cluster to test on.

It builds every `Kustomization` and `HelmRelease` the way the Flux controllers do,
works out what each one needs from the others, and checks the result. A typical
repository takes about a second.

fluxlint is young: expect new rules, and new findings on old repositories, with every
release. Two things will not move under you. A rule ID and its name, once released, are
never changed or reused, and a test in this repository enforces it. The configuration
file is read strictly, so a release that changes it fails loudly instead of ignoring a
key. Pin a version in CI; `install.sh` verifies the checksum.

## Features

* builds Kustomizations with the kustomize SDK, including Flux's patches, generated
  `kustomization.yaml` and `postBuild` substitution
* renders HelmReleases with the Helm SDK, using your values and the pinned chart version
* fetches the Git repositories, OCI artifacts and Helm charts your manifests point at,
  and caches them
* finds bootstrap deadlocks: orderings that can never converge from an empty cluster
* finds missing namespaces, CRDs, sources and substitution variables
* finds pods that reference a Secret, key, ConfigMap or ServiceAccount nothing creates
* validates custom resources against the CRDs you install, and pods against Pod Security
* checks built-in objects for values the API server rejects: a selector that does not
  match its template, a port out of range, a mount with no volume
* finds applies that a fail-closed webhook will reject while it starts, fields that Flux
  and an autoscaler or cert-manager both write, and chart objects that change on every
  upgrade
* reports what a change will prune, orphan or fail to update
* computes the worst-case bootstrap time and shows which timeouts and `dependsOn`
  entries cause it
* checks your own rules, written in CEL
* compares its verdict with a real cluster's, and reports every failure it did not predict
* reports to the terminal, JSON, GitLab Code Quality, SARIF and GitHub annotations

## Install

Download a release binary for Linux or macOS. The script checks the archive against
the release's `checksums.txt` and installs to `~/.local/bin`:

```bash
curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh
```

Pick the directory and the version with `-b` and a tag:

```bash
curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh -s -- -b /usr/local/bin v0.1.0
```

Windows archives are on the [releases page](https://github.com/phoban01/fluxlint/releases).
With Go installed you can also build from source:

```bash
go install github.com/phoban01/fluxlint/cmd/fluxlint@latest
```

## Get started

Run `check` against the directory your bootstrap Kustomization points at. This is the
`--path` you gave `flux bootstrap`.

```bash
fluxlint check clusters/production
```

fluxlint exits `0` when it finds no errors, `1` when it finds some, and `2` when it
cannot run. Use `--fail-on warning` to be stricter.

```bash
fluxlint check -v                        # also list suggestions
fluxlint check --base origin/main        # gate only on what the change introduces
fluxlint check --format json             # or gitlab, sarif, github
fluxlint graph --format mermaid clusters/production
fluxlint rules                           # list every rule
fluxlint explain FL-G002                 # describe one rule
```

## Examples

### A bootstrap deadlock

`configs` waits for `controllers`. But `controllers` holds a HelmRelease whose
HelmRepository lives in `configs`:

```yaml
# clusters/production/infrastructure.yaml
kind: Kustomization
metadata:
  name: controllers
spec:
  path: ./controllers        # contains HelmRelease/podinfo
  wait: true
---
kind: Kustomization
metadata:
  name: configs
spec:
  path: ./configs            # contains HelmRepository/podinfo
  dependsOn:
    - name: controllers
```

On a running cluster nobody notices, because the HelmRepository already exists. A new
cluster never gets past it.

```
  error   FL-G002 bootstrap-deadlock  HelmRelease/flux-system/podinfo: cannot converge from an empty cluster: cycle between HelmRelease/flux-system/podinfo, flux-system/configs, flux-system/controllers
            at controllers/release.yaml:3
            ready(HelmRelease/flux-system/podinfo) -> ready(flux-system/controllers)  (parent has wait: true)
            ready(flux-system/controllers) -> start(flux-system/configs)  (dependsOn)
            start(flux-system/configs) -> ready(HelmRelease/flux-system/podinfo)  (source HelmRepository/flux-system/podinfo needed by HelmRelease/flux-system/podinfo)
```

### A version bump that breaks a patch

A Kustomization pulls an operator from its own repository and points two of its
environment variables at a Secret, by position:

```yaml
patches:
  - target: {kind: Deployment, name: manager}
    patch: |
      - op: replace
        path: /spec/template/spec/containers/0/env/1/valueFrom/secretKeyRef/name
        value: api-credentials
      - op: replace
        path: /spec/template/spec/containers/0/env/2/valueFrom/secretKeyRef/name
        value: api-credentials
```

Version `v1.1.0` of the operator adds a variable at index 1. Someone bumps the tag
and leaves the patch alone. The patch still applies, but to the wrong variables:

```
  error   FL-R001 unresolved-config-reference  Deployment/operator-system/operator-manager-green: container manager env TELEMETRY_TOKEN needs key "token" of Secret operator-system/api-credentials, but ClusterExternalSecret/api-credentials only defines "SERVICE_TOKEN", "SERVICE_URL"
  error   FL-R001 unresolved-config-reference  Deployment/operator-system/operator-manager-green: container manager env SERVICE_TOKEN references Secret operator-system/manager-api-credentials, which nothing creates
  warning FL-R003 positional-patch  operator-system/operator-green: 2 patch path(s) address list elements by position; an upstream change to the list (typically a version bump) silently retargets them
            Deployment/manager /spec/template/spec/containers/0/env/1/valueFrom/secretKeyRef/name  -> currently "TELEMETRY_TOKEN"
            Deployment/manager /spec/template/spec/containers/0/env/2/valueFrom/secretKeyRef/name  -> currently "SERVICE_URL"
  warning FL-R007 module-skew  operator-system/operator-green: is built against example.test/platform-api v1.1.0, but flux-system/platform-api installs its CRDs at v1.0.0: fields and versions the controller expects may not exist
            bump flux-system/platform-api to at least v1.1.0, in a change that lands before this one
```

The last finding comes from the operator's `go.mod`: it was built against a newer API
module than the CRDs pinned next to it.

### Where the bootstrap time goes

fluxlint treats `dependsOn`, `wait`, `timeout` and `retryInterval` as a scheduling
problem and finds the longest path through it. With `-v` it also suggests what to cut:

```
  info    FL-T001 critical-path  worst-case bootstrap bound is 8m0s
            t<=0s       start(flux-system/a)      +0s      created by parent
            t<=5m0s     ready(flux-system/a)      +5m0s    apply and health timeout
            t<=5m30s    start(flux-system/b)      +30s     dependsOn
            t<=7m30s    ready(flux-system/b)      +2m0s    apply and health timeout
            t<=8m0s     start(flux-system/c)      +30s     dependsOn
  info    FL-T002 dominant-delay  flux-system/a: timeout of 5m0s is 62% of the worst-case bound (8m0s) and sits on the critical path
            at clusters/prod/all.yaml:3
  info    FL-T004 unjustified-dependency  flux-system/c: dependsOn flux-system/b, but nothing rendered in flux-system/c imports anything from flux-system/b
            at clusters/prod/all.yaml:37
            on the critical path: removing it lowers the worst-case bound by 30s
  info    FL-T005 redundant-dependency  flux-system/c: dependsOn flux-system/a is already implied by another dependsOn path
            at clusters/prod/all.yaml:37
```

The bound is what Flux allows, not what usually happens. Give fluxlint the reconcile
durations your controllers report and it adds an expected time:

```bash
kubectl -n flux-system port-forward deploy/kustomize-controller 8080 &
curl -s localhost:8080/metrics | grep gotk_reconcile_duration_seconds > observed.prom
fluxlint check --observed observed.prom
```

Set `timing.maxBootstrapBound` to fail a change that pushes the bound past a budget.

### Merge requests

```bash
fluxlint check --base origin/main
```

fluxlint renders both commits. It reads the base with `git archive`, so your working
tree is not touched. Then it:

* fails only on findings the change introduces. Old findings are counted but do not
  block, so you can make the check required on the first day.
* lists objects Flux will prune, and warns when they include namespaces, CRDs, PVCs,
  StatefulSets or Secrets
* lists objects left behind because their Kustomization has `prune: false`
* reports updates to immutable fields, such as a Deployment's selector, which the API
  server will reject unless the Kustomization sets `force: true`
* lists objects that move from one Kustomization to another

This works through charts too. If a chart upgrade changes a selector, the finding
points at the HelmRelease whose version you bumped.

### CI

Each finding carries the file and line to fix. If the object comes from a chart or
another repository, that is the HelmRelease or Kustomization that pulls it in.

```yaml
# GitLab: findings appear in the merge request widget
fluxlint:
  script:
    - fluxlint check --base origin/$CI_MERGE_REQUEST_TARGET_BRANCH_NAME
        --format gitlab --output gl-code-quality-report.json
  artifacts:
    when: always
    reports:
      codequality: gl-code-quality-report.json
  cache:
    key: fluxlint-sources
    paths: [.fluxlint-cache]          # with sources.cacheDir: .fluxlint-cache
```

```yaml
# GitHub Actions: annotations on the diff, or SARIF for code scanning
- run: fluxlint check --format github
- run: fluxlint check --format sarif --output fluxlint.sarif
```

With `--output`, the report goes to the file and the readable text goes to the job log.

### Your own rules

Rules that only make sense in your repository are CEL expressions in `.fluxlint.yaml`.
fluxlint evaluates them against every rendered object they match, including objects
from charts and other repositories.

```yaml
assertions:
  - name: live colour serves traffic
    match: {kind: Deployment, namespace: shop, name: "shop-*"}
    expr: '!object.metadata.name.endsWith("-" + vars.shop_live) || object.spec.replicas > 0'
    message: the colour named by shop_live is scaled to zero
    mustMatch: true        # fail if a rename leaves this rule matching nothing
```

An expression sees `object`, `component` and `vars`, the post-build variables that
apply to the object. An expression that cannot be evaluated is a failure, so a typo in
a field name does not pass quietly.

### The graph

```bash
fluxlint graph --format mermaid clusters/production   # or dot
```

The graph has one node for each Kustomization and HelmRelease. `dependsOn` edges are
solid, parent-to-child edges are dotted, and needs that fluxlint found are dashed and
labelled. The critical path is bold. Deadlocks are red.

## Configuration

fluxlint reads `.fluxlint.yaml` from the repository root. Every field is optional.

```yaml
entrypoints:
  - clusters/production

kubeVersion: "1.35.0"                   # the version charts are rendered for

# The GitRepository that stands for this repository. This is the default.
repoSource: {kind: GitRepository, name: flux-system, namespace: flux-system}

# Things that exist in the cluster but that nothing in Git creates.
externals:
  namespaces: [tenant-a]
  crdGroups: [example.internal]
  secrets:
    - {namespace: flux-system, name: api-credentials, keys: [token]}
  substitutions:
    - kind: ConfigMap
      name: cluster-info
      variables: [cluster_name, region]
  runtimeCRDs:                          # CRDs a controller installs once it runs
    - group: infrastructure.cluster.x-k8s.io
      providedBy: flux-system/infra-capi-providers

rules:                                  # like golangci-lint: all or none, then pick
  default: all
  disable: [retry-cliff]                # an ID, a name, or a family such as timing
  severity:
    positional-patch: error

timing:
  maxBootstrapBound: 30m
  dependencyRequeue: 30s                # the controllers' --requeue-dependency
```

Keep `externals` short. Everything listed there is something fluxlint takes on trust.

`runtimeCRDs` is for CRDs that no manifest contains. cluster-api-operator, for
example, installs a provider's CRDs when it reconciles an `InfrastructureProvider`.
Unlike `crdGroups`, this entry keeps the ordering check: a consumer with no path from
the provider is reported as `FL-T006`.

## Guides

The documentation is at <https://phoban01.github.io/fluxlint/>.

* [What fluxlint renders](docs/reference/rendering.md): sources, credentials, the cache, Helm,
  and the limits
* [Component contracts](docs/guides/contracts.md): how a controller, chart or image declares the
  CRDs and Secret keys it cannot start without
* [Design](docs/design.md): the model, the rules and the roadmap

## Development

```bash
make test   # unit tests; no network
make e2e    # the built binary against e2e/testdata/platform
make lint
make snapshot   # every release target into dist/, nothing published
```

The end-to-end suite runs the binary against a small platform repository built from
pinned public upstreams: cert-manager, kyverno, cluster-api-operator with the AWS
provider, and podinfo. The baseline must be clean. Every other test plants one defect
and checks that fluxlint names it.

`e2e/operators_test.go` does the same for a private platform, with no network. It
serves an API repository and an operator repository over `file://`, CRDs from an
in-process OCI registry, and a Helm repository behind basic auth.

## Licence

fluxlint is [Apache 2.0 licensed](LICENSE).
