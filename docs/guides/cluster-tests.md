# Run beside a cluster test

Many Flux repositories are tested by starting a throwaway cluster in CI, installing
Flux, pointing it at the branch and waiting until every Kustomization and HelmRelease is
Ready. Such a test takes tens of minutes and fails for reasons that have nothing to do
with the change. It also sees things fluxlint cannot. This page says which, and how to
use the two together until you know whether you still need both.

## What each one proves

A cluster test asserts one thing: on a fresh cluster, everything became Ready. Behind
that are several separate claims.

| Claim | Cluster test | fluxlint |
| --- | --- | --- |
| every overlay builds | yes | yes |
| `dependsOn` and sources resolve, and nothing deadlocks | yes, as a timeout | yes, with the cycle and the reason for each edge |
| namespaces and CRDs exist before they are used | yes | yes |
| substitution variables resolve | yes | yes |
| charts render with the values given | yes | yes |
| objects pass the API server's validation | yes, in full | custom resources in full; built-in kinds in part (`FL-V003`, `FL-V004`) |
| pods find their Secrets, keys and ServiceAccounts | for the pods it starts | yes (`FL-R001`, `FL-R002`), and what the controller's code needs if it ships a [contract](contracts.md) |
| an apply is not rejected by a webhook that is still starting | sometimes: it is a race | yes (`FL-R008`) |
| a second reconcile changes nothing | if the test reconciles twice | the known causes (`FL-G003`, `FL-R009`, `FL-R010`) |
| the change can be applied over what is running | **no**: the cluster is new | yes, with `--base` (`FL-D002`, `FL-D003`) |
| images exist, for the platform the nodes run | yes, as `ImagePullBackOff` and a timeout | yes, by asking the registry (`FL-X004`, with `images.verify`) |
| containers stay up | for the pods it starts | **no**; a [contract](contracts.md) test in the component's own CI covers the usual cause |
| the repository's admission policies admit its objects | yes, on create | yes: ValidatingAdmissionPolicies with the API server's code (`FL-V005`), Kyverno policies with Kyverno's CLI (`FL-V007`). With `--base`, on update too, where immutability rules speak |
| webhooks from outside the repository admit the objects | yes | **no** |
| a controller's output is correct | yes | **no** |

Two rows deserve a second look.

A cluster test starts from nothing, so it never applies your change *over* the previous
state. A changed selector, a moved object or a removed namespace passes on a new cluster
and fails on the real one. `fluxlint check --base origin/main` is the only one of the
two that tests the upgrade.

A cluster test usually cannot run the whole repository. It has no cloud account, no
hardware and no real secret store, so its overlay deletes components and its script
seeds Secrets and starts stand-ins by hand. What the test proves is that the *reduced*
repository converges with that help. fluxlint reads the repository as it is.

## What fluxlint does about races and re-applies

These are properties of a running system, and a static tool cannot observe them. It can
find the arrangements that cause them.

**`FL-R008 admission-window`.** A webhook with `failurePolicy: Fail` rejects every
matching request from the moment it is registered until its pods answer. If another
Kustomization applies a matching object in that window, the apply fails and waits a
`retryInterval`. fluxlint evaluates the webhook's rules and selectors against every
rendered object, and reports a component that is neither applied before the webhook
exists nor after the webhook's component is healthy. It also reports the quieter
mistake: `dependsOn` a Kustomization that has no `wait: true`, which is Ready as soon as
it is applied.

**`FL-R013 single-pod-gate`.** Ordering only covers the first start. If one pod serves a
fail-closed webhook that other components' applies go through, every restart of that
pod rejects them all. In 200 passing runs of one real cluster test, 55 hit a webhook
that refused connections, and 50 of those were a single-replica Kyverno. Kyverno's
webhooks are in no manifest, because it registers them when it starts. fluxlint works
them out from the chart and the policies.

**`FL-R009 contested-field`.** Flux applies server-side and takes back every field in
the manifest on each reconcile. If an autoscaler owns `spec.replicas`, or cert-manager
injects a `caBundle`, the two write the field in turn for ever. The first install looks
healthy. This is what a second reconcile pass in a cluster test exists to find.

**`FL-R010 unstable-render`.** fluxlint renders a chart twice when a template calls
`randAlphaNum`, `genCA`, `now` or the like, and reports the objects that differ. Each
upgrade of such a release replaces the password or the certificate.

**`FL-G003 dual-ownership`.** Two Kustomizations that render the same object overwrite
each other on every reconcile.

## Let the cluster check fluxlint

You do not have to take the table on trust. Keep the cluster test, and have it write
down what it saw when it finishes, pass or fail:

```bash
kubectl get kustomizations.kustomize.toolkit.fluxcd.io,helmreleases.helm.toolkit.fluxcd.io \
  -A -o json > cluster-state.json
```

Then give that file to fluxlint, for the same commit and the entrypoint the cluster
ran:

```bash
fluxlint check --cluster-state cluster-state.json clusters/staging
```

```
  error   FL-O001 unpredicted-failure  flux-system/apps: is not Ready in the cluster, and no finding predicted it
            HealthCheckFailed: timeout waiting for: [Deployment/apps/web status: 'InProgress']
  info    FL-O002 unconfirmed-finding  flux-system/monitoring: is Ready in the cluster, although FL-R001 reported an error on it: …
```

`FL-O001` is a failure the cluster found and fluxlint did not. A component that failed
only because a dependency or a child failed is not reported again. Each `FL-O001` is one
of three things:

- a flake in the test. Run it again.
- something only a cluster can see, such as a container that crashes. It belongs to
  the last three rows of the table.
- a gap in fluxlint. Close it with an [assertion](assertions.md), a
  [contract](contracts.md), or an issue.

`FL-O002` is the opposite: fluxlint reports an error, and the cluster was content.
Either the finding is wrong, or the test helped the cluster with something that is not
in Git. A Secret the test script seeds is the usual cause, and production will not have
the script.

The same file from a staging cluster works as well as one from CI.

## Deciding

Run both for a few weeks and count.

- If every cluster-test failure was either predicted by fluxlint or a flake, the cluster
  test is costing you its runtime and proving nothing more on merge requests. Move it to
  a nightly job, where it still tests that containers stay up, and let fluxlint gate
  merge requests.
- If `FL-O001` keeps finding real problems of a kind you cannot express as a rule, keep
  the cluster test on merge requests, and run fluxlint first so that the quick failures
  do not cost a cluster.

Either way fluxlint runs first. A deadlock or a missing variable found in a second is
one cluster that was never started.
