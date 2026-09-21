# Design

This page describes the model behind fluxlint. To use it, start with [Get started](get-started.md).

## 1. Goal

Answer, from the Git tree alone and in **under 10 seconds**, the question a
throw-away cluster test answers in 20–40 minutes:

> If Flux reconciles this commit, will it converge — and how long can it take?

fluxlint is a linker and a scheduler analyser for Flux repositories. It does not
start a cluster, a container, or a controller. It is general purpose: nothing in
the core knows about any particular repository layout, operator, or company.

### Non-goals

- Proving that workloads behave correctly once running: that an image exists, that a
  container stays up, that a controller's output is right. A canary environment or a
  cluster test does that. What fluxlint does claim is narrower and checkable: whether
  the repository *can* converge, and what reconciling a change will do. Where the two
  overlap, `--cluster-state` compares fluxlint's verdict with a real cluster's and
  reports every failure it did not predict ([Run beside a cluster
  test](guides/cluster-tests.md)).
- Replacing policy engines. fluxlint can *invoke* policy evaluation over the
  rendered objects, but the rules it owns are about convergence, not style.

## 2. Model

### 2.1 Rendering

Starting from one or more **entrypoints** (the directory a cluster's root Flux
Kustomization points at — the `--path` given to `flux bootstrap`), fluxlint renders
exactly what kustomize-controller would apply:

1. `kustomize build` in-process (kustomize `krusty` API), generating a
   `kustomization.yaml` where Flux would generate one.
2. Flux-level `spec.patches`, `spec.images`, `spec.targetNamespace`,
   `spec.namePrefix/nameSuffix`, `spec.components`.
3. `postBuild.substitute` / `substituteFrom` using Flux's own envsubst semantics.
4. Recurse into every Flux `Kustomization` found in the output.
5. For sources other than the repo itself (`GitRepository`, `OCIRepository`,
   `HelmRepository`+chart), resolve the pinned ref and render from a
   **content-addressed cache**. A floating ref (`branch: main`, semver range) is
   rendered at its current resolution and reported as non-reproducible.
   `HelmRelease` is expanded with `helm template` (Helm SDK) using the release's
   real values.

Fidelity rule: wherever an upstream Go implementation exists (kustomize, Flux
substitution, Helm, Pod Security admission, apiextensions/CEL validation), fluxlint
calls it rather than re-implementing it. A linter that disagrees with the cluster is
worse than a slow one.

### 2.2 Components, exports and imports

Every Flux `Kustomization` and `HelmRelease` is a **component** with:

- **exports**: Namespaces, CRDs (group, kind, served versions, schema), Secrets and
  ConfigMaps *with their keys*, ServiceAccounts, Services, sources, webhook
  configurations.
- **imports**: whatever its objects reference — the namespace an object lives in,
  the CRD of every custom resource, `secretKeyRef`/`envFrom`/volumes/
  `imagePullSecrets`, `serviceAccountName`, `roleRef`/subjects, `sourceRef`,
  `substituteFrom`, webhook `clientConfig.service`, and — for controllers — the API
  groups named in their RBAC rules (the generated ClusterRole of an operator is, in
  practice, its declared list of watched types).

Things produced outside Git (a bootstrap Secret, a namespace created by an operator
at runtime) are declared once in `.fluxlint.yaml` under `externals:` so they are
explicit, reviewed, and greppable.

Secret-like exports are derived from their producers: `ExternalSecret.spec.target`
and `data[].secretKey`, `ClusterExternalSecret` namespace selectors evaluated
against rendered namespace labels, cert-manager `Certificate.spec.secretName`.

### 2.3 The precedence graph

Each component `K` contributes two events, `start(K)` (first apply) and `ready(K)`.
Edges mean "cannot happen before":

| Edge | Reason |
| --- | --- |
| `start(K) → ready(K)` | apply precedes readiness |
| `ready(D) → start(K)` | `K.spec.dependsOn` contains `D` |
| `start(P) → start(C)` | `C` is an object applied by `P` |
| `ready(C) → ready(P)` | `P` has `wait: true` / health-checks `C` |
| `start(X) → start(K)` | `K` imports a namespace / SA / source exported by `X ≠ K` |
| `ready(X) → start(K)` | `K` imports a CRD exported by `X ≠ K` (server-side dry-run is all-or-nothing per Kustomization) |
| `start(X) → ready(K)` | a health-checked object in `K` needs something from `X` to become healthy (HelmRelease → its HelmRepository, Pod → its Secret) |

Splitting start from ready is what makes parent/child nesting, `wait: true`, and
"applies fine but never becomes healthy" expressible in one graph.

**Bootstrap theorem.** The repository converges from an empty cluster iff this
graph is acyclic. Cycles are found with Tarjan's SCC in O(V+E); the report prints
the cycle with the reason on every edge.

Imports satisfied *only* by retry (an import edge with no declared ordering) are
legal in Flux — it converges by failing and retrying — so they are not deadlocks.
They are a timing cost, handled in §4.

### 2.4 Transitions

With `--base <ref>` fluxlint renders two commits and analyses the delta:
objects pruned (and whether their owner has `prune: true`), immutable-field changes
(selectors, `volumeClaimTemplates`, Job templates), ownership moving between
components, CRD versions removed while custom resources still use them, and the set
of components whose rendered output changed (which also drives incremental
analysis).

## 3. Correctness rules

Rules come in families, and the [rules reference](reference/rules.md) lists every one.

| Family | Question |
| --- | --- |
| Graph | Can the components converge at all? References, ownership, ordering. |
| Substitution | Do post-build variables resolve, and from where? |
| Validation | Will the API server accept what is rendered? |
| Runtime | Can pods start once their objects are applied? Will an apply be rejected by a webhook that is still starting, and will the second reconcile undo the first? |
| Contracts | Does the repository give each controller what it says it needs? |
| Assertions | Do the repository's own rules hold? |
| Transitions | What does reconciling this change do to a cluster that runs the base? |
| Timing | How long can a bootstrap take, and why? |
| Observed | Given what a real cluster reported for this commit, which failures did no rule predict? |

**Contracts (optional, strongest)** — a component may ship a `fluxlint-contract.yaml`
at its tag declaring `requires:` (CRDs+versions, env/secret keys, namespaces) and
`provides:`. The component's own CI proves the contract (e.g. start the manager
under envtest with only the declared CRDs). fluxlint then composes contracts instead
of inferring imports: assume/guarantee reasoning, with the expensive runtime test
moved to where it is cheap and attributable.

## 4. Timing analysis (max-plus)

The precedence graph is a timed event graph. In the max-plus semiring
(`⊕ = max`, `⊗ = +`) event times satisfy

    x = A ⊗ x ⊕ b        ⇒        x = A* ⊗ b

where `A[i][j]` is the delay on edge `j → i`. `A*` exists iff the graph has no
positive-weight cycle — the same condition as §2.3, so deadlock detection and timing
are one computation. On a DAG `A* ⊗ b` is the longest path; fluxlint computes it
with predecessors, then a backward pass for latest-times, giving classic
critical-path quantities per event: earliest, latest, **slack**.

### 4.1 Weights

Every delay is an interval `[best, worst]`; the analysis runs on both ends.

| Delay | best | worst |
| --- | --- | --- |
| apply + health of `K` (`wait`/`healthChecks`) | observed or 0 | `spec.timeout` (defaults to `spec.interval`) |
| `dependsOn` hop | 0 | controller `--requeue-dependency` (30s default) — dependents *poll* |
| import satisfied only by retry | `retryInterval` | `retryInterval × layers`; **`interval`** when `retryInterval` is unset |
| HelmRelease | observed or 0 | `timeout × (1 + remediation.retries)` |
| source pickup | 0 | source `spec.interval` (0 with a webhook Receiver) |

Observed durations are optional and never fetched implicitly:
`--observed <file>` accepts a `kubectl get kustomizations,helmreleases -A -o json`
dump or a Prometheus export of `gotk_reconcile_duration_seconds`. Without it the
output is a **bound**, not a prediction, and is labelled as such.

### 4.2 Outputs

- `FL-T001` **critical path** for bootstrap, with the contribution of every event.
- `FL-T002` **dominant delay**: one component contributes more than *p*% of the
  bound (typically an over-generous `timeout` under a `wait: true` parent).
- `FL-T004` **unjustified dependency**: a `dependsOn` edge for which no import exists
  between the two subtrees. Ranked by critical-path saving if removed; zero-slack
  edges first. Confidence is lowered when either side is opaque (unrendered external
  source, no contract), because needs that only exist at runtime are invisible.
  Always a suggestion.
- `FL-T005` **transitively redundant dependency** (transitive reduction of the
  `dependsOn` graph). Harmless to timing; reported as noise reduction.
- `FL-T006` **implicit ordering**: an import with no declared order — converges only
  by failing and retrying. Reports expected penalty; suggests the missing
  `dependsOn`.
- `FL-T007` **retry cliff**: no `retryInterval` and a long `interval` — one failed
  apply stalls the component for a full interval.
- `FL-T008` **timeout inversion**: `timeout(P)` of a waiting parent is shorter than
  the bound of what it waits for, so `P` flaps NotReady while its children are still
  legitimately converging; also `timeout > interval`.
- `FL-T010` **stale substitution**: a component substitutes from a ConfigMap/Secret that
  another component of the same source applies, with no `dependsOn` path to it. Verified
  against kustomize-controller v1.4 (Flux 2.4): `checkDependencies` holds a dependent back
  until a same-source dependency's `lastAppliedRevision` matches ("dependency revision is
  not up to date"), and only sources are watched, so without the `dependsOn` the reader
  may substitute old values and not run again for an `interval`. Skipped when the object
  carries `reconcile.fluxcd.io/watch: Enabled` (Flux ≥ 2.7).
- **Budget gate**: `timing.maxBootstrapBound: 30m` turns the bound into a CI
  regression check. Timing findings are otherwise advisory and never fail a build.

`fluxlint graph --format dot|mermaid` renders the graph with the critical path
highlighted.

## 5. Architecture

```
cmd/fluxlint   CLI: check, graph, explain, rules, version
pkg/config     .fluxlint.yaml
pkg/source     Git, OCI and Helm resolution; content-addressed cache
pkg/render     kustomize (krusty), Flux post-processing, helm template, build cache
pkg/model      components, objects, contracts
pkg/graph      event graph, SCC, longest path, slack
pkg/lint       the rules, one file per family
pkg/report     text, JSON, SARIF, GitLab Code Quality, GitHub annotations
```

- One static Go binary. No cluster access. The network is used only to fetch the
  sources the manifests pin, and the cache makes later runs offline.
- Charts render concurrently. kustomize builds run one at a time, because the
  kustomize SDK is not safe to run in parallel.
- With `--base`, and across entrypoints, a build is reused when everything kustomize
  read the first time is unchanged: every file, every directory listing, every path it
  probed and found missing. A reused build is indistinguishable from a fresh one, and a
  test holds it to that.
- Repository-specific rules stay out of the core. The `assertions` block evaluates CEL
  over rendered objects and resolved variables, so users encode their own conventions
  without forking.

## 6. Testing

- Fixtures are small synthetic repositories. No real-world manifests are vendored.
- A guard test fails when a rule in the catalogue has no test.
- Differential tests build the same directories with fluxlint and with
  `fluxcd/pkg/kustomize`, the package kustomize-controller uses, and compare the output.
- Property tests cover the graph package: longest path against brute force on random
  DAGs, and planted cycles are always found.
- The end-to-end suite runs the built binary against a platform repository made from
  pinned public upstreams, and against a private platform mocked in process: Git over
  `file://`, an OCI registry, a Helm repository behind basic auth. Each test plants one
  defect and checks that fluxlint names it.

## 7. Roadmap

Done:

| Area | Scope |
| --- | --- |
| Rendering | in-process kustomize with Flux's overlay and substitution; `.sourceignore` and `spec.ignore`; `GitRepository`, `OCIRepository` and `HelmRepository` sources with a cache; HelmReleases through the Helm SDK with `valuesFrom`, post-renderers, hooks and chart dependencies |
| Graph | dangling references, dual ownership, bootstrap deadlocks through namespaces, CRDs, sources and runtime needs |
| Validation | custom resources against rendered CRDs, including CEL rules; built-in kinds; Pod Security |
| Runtime | Secret, ConfigMap, key, ServiceAccount and pull-secret resolution; webhook backends; positional patches; contracts; Go module skew |
| Timing | critical path, slack, dependency review, observed durations, budget gate, graph export |
| Transitions | `--base`, new-findings-only gating, prune, immutable fields, ownership moves, orphans, incremental rendering |
| Assertions | CEL over rendered objects with inherited variable scope |
| Reporting | text, JSON, GitLab Code Quality, SARIF, GitHub annotations, with file and line |

Before 1.0:

- freeze rule IDs and the configuration format
- publish the Flux versions each release is tested against
- `Bucket` sources, if someone needs them

## 8. Known limits

- Controller logic is invisible without a contract.
- Admission webhooks that a controller registers at runtime (policy engines), and
  webhooks with `matchConditions`, are not evaluated.
- Races are found by their cause (an ordering that leaves a window), not observed.
  A race with a cause fluxlint does not model is invisible; `--cluster-state` is how
  such a gap gets noticed.
- Anything produced at runtime must be declared in `externals`; an over-broad
  externals list silently weakens the analysis, so its size is reported.
- Timing output is a bound unless observed durations are supplied.
- Floating refs make results non-reproducible; fluxlint reports them rather than
  pretending otherwise.
