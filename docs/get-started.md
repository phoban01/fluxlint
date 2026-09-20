# Get started

## Run it

fluxlint needs one thing from you: the directory your bootstrap Kustomization points
at. This is the `--path` you gave `flux bootstrap`, and fluxlint calls it an
*entrypoint*.

```bash
cd my-fleet-repo
fluxlint check clusters/production
```

From there fluxlint follows every Flux `Kustomization` and `HelmRelease` it finds,
fetching the Git repositories, OCI artifacts and charts they point at. The first run
downloads them. Later runs use the cache and take about a second.

## Read the output

```
clusters/production: 12 components (1 not rendered), 214 objects
  error   FL-G002 bootstrap-deadlock  HelmRelease/flux-system/podinfo: cannot converge from an empty cluster: cycle between HelmRelease/flux-system/podinfo, flux-system/configs, flux-system/controllers
            at controllers/release.yaml:3
            ready(HelmRelease/flux-system/podinfo) -> ready(flux-system/controllers)  (parent has wait: true)
            ready(flux-system/controllers) -> start(flux-system/configs)  (dependsOn)
            start(flux-system/configs) -> ready(HelmRelease/flux-system/podinfo)  (source HelmRepository/flux-system/podinfo needed by HelmRelease/flux-system/podinfo)
  warning FL-T007 retry-cliff  flux-system/apps: no retryInterval: a failed apply is not retried for 30m0s (interval)
            at clusters/production/apps.yaml:3
  info    FL-T001 critical-path  worst-case bootstrap bound is 12m30s

1 errors, 1 warnings, 6 suggestions (use -v to list suggestions) in 1.04s
```

The first line counts *components*: each Kustomization and each HelmRelease is one.
A component that could not be rendered is counted too, because rules that depend on
its contents are then less sure of themselves.

Each finding has:

* a severity: `error` fails the run, `warning` does not unless you pass
  `--fail-on warning`, and `info` is a suggestion shown in full with `-v`
* a rule ID and name. `fluxlint explain FL-G002` describes the rule.
* the component and, where it applies, the object
* the file and line to fix. When the object comes from a chart or another repository,
  this is the HelmRelease or Kustomization in your repository that pulls it in.
* detail lines. For a deadlock these are the edges of the cycle. `start(x)` is the
  moment Flux first applies `x`, and `ready(x)` is the moment it reports Ready.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | no findings at or above `--fail-on` |
| `1` | findings at or above `--fail-on` |
| `2` | fluxlint could not run: bad flags, unreadable config, no entrypoint |

## Tell fluxlint what Git does not contain

Most clusters have a few things that no manifest creates: a namespace made by the
cloud provider, a Secret written during bootstrap, a ConfigMap of cluster facts that
`postBuild.substituteFrom` reads. fluxlint reports these as missing until you list
them:

```yaml
# .fluxlint.yaml
entrypoints:
  - clusters/production

kubeVersion: "1.35.0"

externals:
  namespaces: [tenant-a]
  secrets:
    - {namespace: flux-system, name: sops-age, keys: [age.agekey]}
  substitutions:
    - kind: ConfigMap
      name: cluster-info
      variables: [cluster_name, region]
```

Keep this list short. Everything on it is something fluxlint takes on trust. With
`entrypoints` set, `fluxlint check` needs no arguments.

See the [configuration reference](reference/configuration.md) for every field.

## Next steps

* [Check merge requests](guides/merge-requests.md), and fail only on what a change
  introduces.
* [Run in CI](guides/ci.md) with findings shown on the diff.
* [Shorten bootstrap time](guides/timing.md).
