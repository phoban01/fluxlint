# Configuration

fluxlint reads `.fluxlint.yaml` from the repository root, or the file you name with
`--config`. Every field is optional, and a missing file means defaults. An unknown key
is an error, so a misspelt setting cannot silently do nothing.

```yaml
entrypoints:
  - clusters/production

kubeVersion: "1.35.0"

repoSource: {kind: GitRepository, name: flux-system, namespace: flux-system}

externals:
  namespaces: [tenant-a]
  crdGroups: [example.internal]
  secrets:
    - {namespace: flux-system, name: sops-age, keys: [age.agekey]}
  substitutions:
    - kind: ConfigMap
      name: cluster-info
      variables: [cluster_name, region]
  runtimeCRDs:
    - group: infrastructure.cluster.x-k8s.io
      providedBy: flux-system/capi-providers

sources:
  cacheDir: .fluxlint-cache
  overrides:
    - {kind: GitRepository, name: my-operator, path: ../my-operator}

rules:
  default: all
  disable: [retry-cliff, timing]
  enable: [critical-path]
  severity:
    positional-patch: error

assertions: []

timing:
  maxBootstrapBound: 30m
  dependencyRequeue: 30s
  dominantShare: 0.4
```

## entrypoints

The directories your bootstrap Kustomizations point at, relative to the repository
root. Entrypoints on the command line replace this list.

Clusters in one repository differ. They run different Kubernetes versions, and a
namespace that exists out of band in one does not exist in another. An entry can be an
object that carries settings for that cluster alone:

```yaml
kubeVersion: "1.35.0"
externals:
  namespaces: [shared]

entrypoints:
  - clusters/production
  - path: clusters/staging
    kubeVersion: "1.36.0"              # staging upgrades first
    externals:
      namespaces: [sandbox]            # added to the shared list
    timing:
      maxBootstrapBound: 45m
```

| Field | Effect |
| --- | --- |
| `kubeVersion`, `repoSource` | replace the top-level value |
| `externals` | added to the top-level externals |
| `timing` | each field that is set replaces the top-level one |

`rules`, `assertions` and `sources` are shared by every entrypoint. A path given on the
command line picks up the settings of the entry with the same path.

## kubeVersion

The Kubernetes version charts are rendered for. Charts read it as
`.Capabilities.KubeVersion` and many refuse to render for a version they do not
support, so set it to what your clusters run. The default is `1.35.0`.

It is also the release built-in objects are checked against (`FL-V003`). Each Kubernetes
release publishes the OpenAPI schema of its APIs, and fluxlint validates every
Deployment, Service, Job and so on against the schema of this release: a field that
release does not have is an error, and so is an API version it does not serve. Clusters
on different versions each get their own answer, because an
[entrypoint](#entrypoints) can set its own `kubeVersion`.

To see what an upgrade will break, run the check once more with the version you are
moving to:

```yaml
entrypoints:
  - path: clusters/production
    kubeVersion: "1.36"
```

`1.35`, `1.35.2` and `v1.35.2` are all accepted. Without a patch number `.0` is used,
since a minor version's API does not change.

## repoSource

The Flux source object that stands for this repository. A Kustomization whose
`sourceRef` names it is rendered from your working tree. Any other source is fetched.
The default is the `GitRepository` named `flux-system` in `flux-system`, which is what
`flux bootstrap` creates.

## externals

Things that exist in the cluster but that nothing in Git creates. Keep the list short:
everything on it is something fluxlint takes on trust.

| Field | Meaning |
| --- | --- |
| `namespaces` | namespaces that already exist |
| `crdGroups` | API groups whose CRDs are installed by something fluxlint cannot see. Objects in these groups are not validated and need no ordering. |
| `secrets` | Secrets created out of band. `keys` is optional. When you give it, key references are checked against it. |
| `substitutions` | ConfigMaps and Secrets read by `postBuild.substituteFrom` that Git does not contain, with the variables they provide |
| `runtimeCRDs` | API groups whose CRDs appear only once a component is running |

### runtimeCRDs

Some CRDs are in no manifest. cluster-api-operator, for example, installs a provider's
CRDs when it reconciles an `InfrastructureProvider`. Name the group and the component
that causes the install:

```yaml
externals:
  runtimeCRDs:
    - group: infrastructure.cluster.x-k8s.io
      providedBy: flux-system/capi-providers      # or HelmRelease/<namespace>/<name>
```

Unlike `crdGroups`, this keeps the ordering check. A consumer with no path from the
provider is reported as `FL-T006`.

## sources

| Field | Meaning |
| --- | --- |
| `cacheDir` | where fetched sources are kept, relative to the repository root. The default is the user cache directory. `--cache-dir` wins over both. |
| `overrides` | use a local directory in place of a source, for example a sibling checkout in CI. `path` is relative to the repository root. `namespace` is optional. |
| `kubernetesSchemas` | where the API schemas of Kubernetes releases are read from: a URL prefix or a directory, laid out like the Kubernetes repository (`<base>/v1.35.0/api/openapi-spec/v3/`). The default is the Kubernetes repository on GitHub. A schema is fetched once per release and cached, so later runs and `--offline` runs need no network. Point it at a mirror or a checked-in directory when CI cannot reach GitHub. |

See [What fluxlint renders](rendering.md) for credentials and cache behaviour.

## rules

Choose which rules run, the way golangci-lint chooses linters: start from all of them
or none, then turn single rules on or off.

| Field | Meaning |
| --- | --- |
| `default` | `all` (the default) or `none` |
| `disable` | rules to turn off. Use it with `default: all`. |
| `enable` | rules to turn on. Use it with `default: none`. |
| `severity` | a rule's severity: `error`, `warning` or `info` |

Name a rule by its ID (`FL-T007`) or by its name (`retry-cliff`). In `enable` and
`disable`, a family name stands for every rule in the family: `graph`, `substitution`,
`validation`, `runtime`, `contracts`, `assertions`, `transitions`, `sources` or `timing`.
`fluxlint rules` lists each rule with its family.

A rule named on its own beats its family, so you can switch a family off and keep one
rule from it:

```yaml
rules:
  disable: [timing]
  enable: [critical-path]
```

Or run only what you choose:

```yaml
rules:
  default: none
  enable: [graph, validation, FL-R001]
```

A name that is not a rule or a family is an error, so a typo cannot leave a rule
running that you meant to turn off. The [rules reference](rules.md) lists every rule
and its default severity.

## assertions

Your own rules, written in CEL. See [Write your own rules](../guides/assertions.md).

## contracts

```yaml
contracts:
  images:
    - registry.example.com/platform/*
```

`images` lists patterns for container images that may carry a
[contract](../guides/contracts.md#attach-the-contract-to-the-image). `*` matches any
run of characters. Only images that match are looked up. With no patterns, fluxlint
never asks a registry about an image.

## kyverno

```yaml
kyverno:
  command: /usr/local/bin/kyverno
```

When the repository installs Kyverno policies that validate, fluxlint gives them and
every rendered object to the `kyverno` CLI and reports what an `Enforce` policy rejects
as `FL-V007`. `command` is the CLI to run, and defaults to `kyverno` on `PATH`.

The CLI is Kyverno's engine in one binary. Kyverno cannot be linked into fluxlint: it
builds against its own forks of the Kubernetes libraries. Running the CLI also means
the verdict comes from the version you install, which can be the version your clusters
run.

fluxlint never downloads or starts anything. If the CLI is not there, the run says how
many policies were not evaluated (`-v`). Get the CLI from the
[Kyverno releases](https://github.com/kyverno/kyverno/releases).

ValidatingAdmissionPolicies need no setup. fluxlint evaluates them in process, with the
API server's own code (`FL-V005`).

## images

```yaml
images:
  verify:
    - registry.example.com/*
  platforms: [linux/amd64]
```

`verify` lists patterns for container images that must exist in their registry. `*`
matches any run of characters, so `"*"` alone checks every image. For each match
fluxlint asks the registry whether the tag or digest is there, with the credentials in
your Docker config, and reports `FL-X004` if it is not. This catches a version bump to a
tag that was never pushed, which a cluster would only show as `ImagePullBackOff`.

`platforms` names what your nodes run. A multi-platform image that lacks one of them is
reported too.

An image that was found is cached, so later runs and `--offline` runs do not ask again.
One that was missing is asked about on every run. If the registry cannot be reached or
refuses the credentials, the image is listed as not checked (`-v`), and is not a
finding. With no patterns, fluxlint never asks a registry about an image.

## timing

| Field | Default | Meaning |
| --- | --- | --- |
| `maxBootstrapBound` | none | fail with `FL-T100` when the worst-case bound is longer than this |
| `dependencyRequeue` | `30s` | how often a controller checks a dependency that is not ready. Set it to your controllers' `--requeue-dependency`. |
| `dominantShare` | `0.4` | the share of the bound above which one component is reported as dominant (`FL-T002`) |

Durations use Go syntax: `90s`, `5m`, `1h30m`.
