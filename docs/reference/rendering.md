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

### What a chart is told about the cluster

Charts ask `.Capabilities` what the cluster is and render differently for the answer:
`policy/v1` or `policy/v1beta1`, a ServiceMonitor or none. helm-controller asks the real
cluster. `helm template` answers from a list built into Helm, which has no kinds and
still has API versions that were removed years ago, so the same chart can render
differently offline.

fluxlint answers with what a cluster of your `kubeVersion` serves, read from the
discovery documents that Kubernetes release publishes (fetched once and cached, like the
API schemas). Alpha versions are left out, since clusters do not serve them unless told
to. APIs that come from CRDs are not in the answer yet: a chart that adds a
ServiceMonitor only when `monitoring.coreos.com/v1` exists renders without it. Releases
older than 1.30 publish no such document, and Helm's list is used; the run says so.

## More than one cluster

A Kustomization or HelmRelease with `spec.kubeConfig` applies to another cluster: the
usual way a management cluster installs add-ons into the clusters it creates. fluxlint
names each target cluster after its kubeconfig Secret and keeps them apart:

* objects in different clusters never meet. cert-manager installed in the management
  cluster and again in a workload cluster is two installations, not one object with two
  owners. A Secret in one cluster does not satisfy a pod in another, and a CRD, a
  namespace, a webhook or an admission policy only counts in the cluster it is in.
* ordering spans all of them. `dependsOn`, `wait` and timeouts live in the cluster Flux
  runs in, so the deadlock check and the timing analysis use the whole graph.
* sources are read where Flux runs, whatever cluster the result is applied to.

A child that a remote Kustomization creates stays in that cluster. The JSON report names
the cluster of every component that is not local.

### What Cluster API delivers

A `ClusterResourceSet` names Secrets or ConfigMaps whose values are manifests, and
Cluster API applies those manifests to the workload clusters of its namespace that its
`clusterSelector` matches. fluxlint follows them. The manifests can be in a Secret or
ConfigMap in Git, or in the `target.template` of the ExternalSecret or
ClusterExternalSecret that produces the Secret, where template actions such as
`{{ .token }}` stand for values that are not known but leave the objects and their keys
readable. What is delivered counts as present in that cluster: a Secret with its keys,
a ClusterIssuer, a namespace.

A workload cluster is matched to its Cluster API `Cluster` by the usual name of its
kubeconfig Secret, `<cluster>-kubeconfig`. When the `Cluster` object is in Git its labels
decide whether the selector matches; when something else creates it, being in the
ClusterResourceSet'"'"'s namespace is enough.

Deliveries are not Flux objects, so they take no part in ordering or timing. What
reaches a workload cluster some other way is not in Git: declare it under `externals`.

## Flux versions

There is no setting for the Flux version, because the repository already says it.
Flux objects are custom resources, and each cluster's Kustomizations and HelmReleases
are checked against the Flux CRDs that cluster installs (its `gotk-components.yaml`).
Two clusters on different Flux releases are each checked against their own.

A field the installed CRD does not define is reported (`FL-V009`): the API server drops it
without an error, so a field from newer Flux documentation, or one copied from a cluster
on a newer release, silently has no effect.

What fluxlint does not track per release is how the controllers build: manifests are
rendered with the kustomize and Helm libraries of current Flux. A repository that does
not keep Flux's manifests in Git (Flux Operator, Terraform) gets no CRD check for Flux
objects.

## Limits

* `Bucket` sources are not fetched.
* Where a spec uses something fluxlint does not model, it says so (`FL-X003`). It does
  not guess.
* A component that cannot be rendered is counted in the summary line. While any are
  missing, "nothing creates this Secret" is a warning marked low confidence, not an
  error, because the missing component may be what creates it.
* A file encrypted with SOPS is not what reaches the API server, because Flux decrypts
  it first. fluxlint cannot decrypt it, so it reads the object's name and keys and
  leaves its values unvalidated.
