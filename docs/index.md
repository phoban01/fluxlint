# fluxlint

fluxlint is a static analyser for [Flux](https://fluxcd.io) repositories. It finds
changes that would stop a cluster from reconciling, before they merge and without a
cluster to test on.

It builds every `Kustomization` and `HelmRelease` the way the Flux controllers do,
works out what each one needs from the others, and checks the result. A typical
repository takes about a second.

```bash
curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh
fluxlint check clusters/production
```

!!! note
    fluxlint is young, and every release adds rules. A rule ID and its name never change
    once released, and the configuration file is read strictly, so an incompatible
    release fails loudly. Pin a version in CI.

## What it finds

**Changes that cannot converge.** A `dependsOn` cycle is easy to spot. A cycle that
runs through a HelmRepository, a namespace or a CRD is not, and a running cluster
hides it, because the missing piece already exists there. fluxlint works from an empty
cluster, so it sees the deadlock the next bootstrap would hit.

**Objects the API server will reject.** Custom resources are validated against the
CRDs your repository installs, including the CRD author's CEL rules. Built-in objects
are decoded strictly, then checked for values the API server refuses: a selector that
does not match its pod template, a port out of range, a volume mount with no volume, a
CronJob schedule that does not parse. Pods are checked against the Pod
Security level of their namespace.

**Pods that cannot start.** A Secret, a key, a ConfigMap, a ServiceAccount or a pull
secret that nothing creates. ExternalSecrets, ClusterExternalSecrets, Certificates
and SealedSecrets count as producers.

**Changes that damage a running cluster.** With `--base`, fluxlint reports what Flux
will prune, which updates the API server will refuse because a field is immutable,
and which objects will be left behind.

**Slow bootstraps.** fluxlint computes the worst-case time from `dependsOn`, `wait`,
`timeout` and `retryInterval`, shows the path that sets it, and points at the
dependencies nothing justifies.

## Where to go next

* [Install](install.md) fluxlint.
* [Get started](get-started.md) with a first run and learn to read the output.
* Make it a [merge request check](guides/merge-requests.md).
* Look up a finding in the [rules reference](reference/rules.md).
