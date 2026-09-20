# Shorten bootstrap time

`dependsOn`, `wait`, `timeout` and `retryInterval` decide how long a new cluster takes
to come up, and how long a bad change takes to fail. fluxlint treats them as a
scheduling problem and finds the longest path through it.

## Read the critical path

```bash
fluxlint check -v clusters/production
```

```
  info    FL-T001 critical-path  worst-case bootstrap bound is 8m0s
            t<=0s       start(flux-system/a)      +0s      created by parent
            t<=5m0s     ready(flux-system/a)      +5m0s    apply and health timeout
            t<=5m30s    start(flux-system/b)      +30s     dependsOn
            t<=7m30s    ready(flux-system/b)      +2m0s    apply and health timeout
            t<=8m0s     start(flux-system/c)      +30s     dependsOn
```

Every component has two events. `start` is the moment Flux first applies it. `ready`
is the moment it reports Ready. Each line gives the latest time the event can happen in
a bootstrap that still succeeds, what that step adds, and why.

The bound is what your settings allow, not what usually happens. A component with
`wait: true` and `timeout: 5m` may be ready in ten seconds, but Flux will wait five
minutes before it gives up, and everything behind it waits too.

## What fluxlint suggests

| Rule | Meaning |
| --- | --- |
| `FL-T002` | one timeout makes up most of the bound and sits on the critical path |
| `FL-T004` | a `dependsOn` that nothing rendered justifies: the dependent uses no namespace, CRD, source or Secret from the dependency |
| `FL-T005` | a `dependsOn` already implied by another path |
| `FL-T006` | the reverse: a component needs something from another and nothing orders them, so it fails and retries until the other catches up |
| `FL-T007` | no `retryInterval`, so a failed apply waits a full `interval` before the next try |
| `FL-T008` | a `wait: true` parent gives up before the things it waits for |
| `FL-T010` | a ConfigMap read by `substituteFrom` changes, but nothing makes its readers reconcile, so they keep the old values until their next interval |

For `FL-T004`, fluxlint says what removing the dependency would save:

```
  info    FL-T004 unjustified-dependency  flux-system/c: dependsOn flux-system/b, but nothing rendered in flux-system/c imports anything from flux-system/b
            at clusters/prod/all.yaml:37
            on the critical path: removing it lowers the worst-case bound by 30s
```

Treat this as a question, not an order. fluxlint sees what is rendered. It cannot see
that a controller in `c` calls a webhook served from `b`. If that is the reason, keep
the dependency, and consider a [contract](contracts.md) so the reason is written down.

## Add real durations

Give fluxlint the reconcile durations your controllers report and it adds an expected
time beside the bound:

```bash
kubectl -n flux-system port-forward deploy/kustomize-controller 8080 &
curl -s localhost:8080/metrics | grep gotk_reconcile_duration_seconds > observed.prom
fluxlint check --observed observed.prom
```

A YAML map works too:

```yaml
flux-system/infrastructure: 95s
flux-system/apps: 20s
HelmRelease/cert-manager/cert-manager: 48s
```

## Set a budget

```yaml
# .fluxlint.yaml
timing:
  maxBootstrapBound: 30m
```

A change that pushes the bound past the budget fails with `FL-T100`. This stops a
bootstrap from growing slower one reasonable-looking timeout at a time.
