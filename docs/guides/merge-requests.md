# Check merge requests

A repository that has run for years will have findings on the first day. `--base`
lets you gate on new ones only.

```bash
fluxlint check --base origin/main
```

fluxlint renders both commits. It reads the base with `git archive`, so your working
tree is not touched. Builds that the change does not affect are done once and shared
between the two, so the second render costs little.

## New findings only

A finding that also exists at the base is marked `existing`. It is counted in the
summary but does not fail the run. You can make fluxlint a required check today and
pay down the old findings later.

Findings are matched by rule, component, object and message, not by line number, so
moving a manifest within a file does not make an old finding look new.

## What the change does to a running cluster

With a base to compare against, fluxlint also reports the transition:

| Rule | Reports |
| --- | --- |
| `FL-D001` | objects Flux will prune. It is a warning when they include namespaces, CRDs, PVCs, StatefulSets or Secrets. |
| `FL-D002` | updates to immutable fields, such as a Deployment's selector, a StatefulSet's `volumeClaimTemplates`, a Job's template or a RoleBinding's `roleRef`. The API server rejects them unless the Kustomization sets `force: true`. |
| `FL-D003` | objects that move from one Kustomization to another |
| `FL-D004` | objects left behind because their Kustomization has `prune: false` |

This works through charts and other repositories. If a chart upgrade changes a
Deployment's selector, the finding points at the HelmRelease whose version you bumped:

```
  error   FL-D002 immutable-change  HelmRelease/apps/podinfo Deployment/apps/podinfo: changes immutable field(s): the apply is rejected and the component stays NotReady until the object is deleted or the Kustomization sets force: true
            at apps/podinfo/release.yaml:8
            spec.selector
```

## Shallow clones

CI systems often fetch a single commit. `--base` needs the base commit too:

```bash
git fetch --depth=1 origin main
fluxlint check --base origin/main
```
