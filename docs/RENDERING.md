# What fluxlint renders

fluxlint checks the objects Flux would apply, so it has to build them the way Flux
does. This page lists what it builds and where it stops.

## Kustomizations

Builds run in process with the kustomize SDK. fluxlint then applies what
kustomize-controller applies on top:

* a generated `kustomization.yaml` for directories that have none
* `patches`, `images`, `components`, `targetNamespace`, `namePrefix` and `nameSuffix`
* `postBuild` substitution with Flux's own `envsubst` package, including the
  `substitute: disabled` annotation

The generated file list leaves out what source-controller leaves out of the artifact:
its default exclusions, anything matched by a `.sourceignore` file, and the source's
`spec.ignore`. A `values.yaml` that never reaches the artifact is not reported as a
broken manifest.

The test suite builds a set of directories with both fluxlint and
`fluxcd/pkg/kustomize`, the package kustomize-controller uses, and fails if the output
differs.

## Other sources

A Kustomization that reads from another `GitRepository` or from an `OCIRepository` is
rendered too. fluxlint fetches the source once, at the ref your manifests pin, and
keeps it in a cache (`~/.cache/fluxlint`, `--cache-dir`, or `sources.cacheDir`). Later
runs need no network.

fluxlint uses the credentials you already have:

| Source | Credentials |
| --- | --- |
| Git | `git` credential helpers and `insteadOf` rules |
| OCI | your Docker config |
| HTTP Helm repository | `$NETRC` or `~/.netrc` |

In GitLab CI the netrc line is
`machine gitlab.example.com login gitlab-ci-token password $CI_JOB_TOKEN`.

| Flag | Behaviour |
| --- | --- |
| *(default)* | use the cache, fetch what is missing |
| `--offline` | never use the network; a cache miss is reported as `FL-X001` |
| `--refresh` | also re-resolve floating refs (branches, semver ranges, `latest`) |

A floating ref is reported as `FL-X002`, because what Flux applies can then change
without a commit to your repository.

If CI has already checked out a sibling repository, point the source at the directory:

```yaml
sources:
  overrides:
    - {kind: GitRepository, name: my-operator, path: ../my-operator}
```

An unreachable host costs one 5 second connect timeout per run, not one per source.

## HelmReleases

Charts are rendered in process with the Helm SDK, as `helm template --include-crds`
would, using:

* the release's `values` and `valuesFrom`, merged as helm-controller merges them.
  `targetPath` follows `--set`, or `--set-string` when the value is quoted.
* the release name and target namespace helm-controller would choose
* the chart version the manifests pin
* `kubeVersion` from `.fluxlint.yaml`. Charts gate on it, so set it to what your
  clusters run.

Charts can come from HTTP(S) and OCI `HelmRepository` objects, from `OCIRepository`
chart refs, and from a directory in a `GitRepository`. For a chart in Git, fluxlint
loads the dependencies listed in `Chart.yaml` from their `file://` path or fetches
them from their repository, as source-controller does before it packages the chart.

`spec.postRenderers` with kustomize patches and images are applied. Install and
upgrade hooks are included, because a pre-install Job is a real pod that needs real
Secrets. Test and delete hooks are left out.

Each release is a node in the graph, with helm-controller's timeout and install
retries. A chart that cannot render with your values is reported as `FL-G008`.

## Limits

* `Bucket` sources are not fetched.
* Where a spec uses something fluxlint does not model, it says so (`FL-X003`). It does
  not guess.
* A component that cannot be rendered is counted in the summary line. While any are
  missing, "nothing creates this Secret" is a warning marked low confidence, not an
  error, because the missing component may be what creates it.
