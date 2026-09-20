# fluxlint — design

Status: draft. Licence: Apache-2.0 (same as Flux).

## 1. Goal

Answer, from the Git tree alone and in **under 10 seconds**, the question a
throw-away cluster test answers in 20–40 minutes:

> If Flux reconciles this commit, will it converge — and how long can it take?

fluxlint is a linker and a scheduler analyser for Flux repositories. It does not
start a cluster, a container, or a controller. It is general purpose: nothing in
the core knows about any particular repository layout, operator, or company.

### Non-goals

- Proving that workloads behave correctly once running (that is what a canary
  environment is for).
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

IDs are stable; every rule has `fluxlint explain <ID>`.

**Graph**
- `FL-G001` dangling `dependsOn` / `sourceRef`
- `FL-G002` bootstrap deadlock (cycle), with per-edge reasons
- `FL-G003` object managed by more than one component
- `FL-G004` import with no exporter anywhere (missing namespace / SA / Secret / source)
- `FL-G005` custom resource whose CRD is exported by no component
- `FL-G006` custom resource uses an `apiVersion` its CRD does not serve
- `FL-G007` `dependsOn` across namespaces / kinds that Flux does not support

**Substitution**
- `FL-S001` `${var}` with no definition and no default (Flux substitutes `""`)
- `FL-S002` `substituteFrom` target not exported and not in `externals`
- `FL-S003` variable defined but never used
- `FL-S004` literal `${...}` in a component with no `postBuild` (left unexpanded)

**Schema and admission**
- `FL-V001` object fails its OpenAPI / CRD schema (including CEL
  `x-kubernetes-validations` where evaluable offline)
- `FL-V002` Pod spec rejected by the namespace's Pod Security level
  (`k8s.io/pod-security-admission` evaluator)
- `FL-V003` Helm values rejected by the chart's `values.schema.json`
- `FL-V004` `ValidatingAdmissionPolicy` / Kyverno policy in the repo rejects an
  object in the repo

**Runtime wiring** (things that apply cleanly and then never run)
- `FL-R001` `secretKeyRef`/`configMapKeyRef` names a key its producer does not define
- `FL-R002` `imagePullSecrets` / `serviceAccountName` not present in the namespace
- `FL-R003` JSON patch addresses a list by index (`/env/3/...`) — fragile across
  upstream version bumps; reports what the index resolves to at the pinned version
- `FL-R004` webhook `clientConfig.service` missing, selector matches no Pod, port
  mismatch, or backing workload scaled to 0 with `failurePolicy: Fail`
- `FL-R005` fail-closed webhook intercepts objects of its own component without
  excluding its own namespace (self-block)
- `FL-R006` controller RBAC names an API group/resource that no CRD in its
  dependency closure exports (the controller will crash on a missing kind)
- `FL-R007` controller built against API module version *N* while the CRDs pinned
  in the cluster are older than *N* (from `go.mod` at the pinned tag, when available)
- `FL-R008` Service selector matches no Pod template
- `FL-R009` image reference does not exist / wrong architecture (network; opt-in,
  cached, parallel HEAD requests)

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
- `FL-T003` **umbrella wait**: a `wait: true` parent with many children makes every
  dependent of the parent wait for its slowest child; suggests depending on the
  specific child instead. Reports the saving.
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
- `FL-T009` **poll depth**: long `dependsOn` chains accumulate one dependency-requeue
  period per hop.
- `FL-T010` **stale substitution** *(candidate — semantics to be verified against
  each supported Flux version)*: a component substitutes from a ConfigMap that is
  applied by a different component on the same source revision without depending on
  it, so a variable change may only take effect one `interval` later.
- **Budget gate**: `timing.maxBootstrapBound: 30m` turns the bound into a CI
  regression check. Timing findings are otherwise advisory and never fail a build.

`fluxlint graph --format dot|mermaid` renders the graph with the critical path
highlighted.

## 5. Architecture

```
cmd/fluxlint            CLI: check | timing | graph | diff | explain
pkg/source              entrypoint discovery, ref resolution, content-addressed cache
pkg/render              kustomize (krusty) + Flux post-processing + helm template
pkg/model               components, exports, imports, externals
pkg/graph               event graph, SCC, longest path / slack, transitive reduction
pkg/rules               one file per rule family; Rule interface; stable IDs
pkg/report              text, JSON, SARIF, GitLab Code Quality, GitHub annotations
```

- Go, single static binary, container image, pre-commit hook. No cluster access, no
  network by default (`--online` enables ref/image existence checks; the cache makes
  repeat runs offline again).
- Rendering is parallel per component; with `--base`, only components whose inputs
  changed are re-rendered.
- Performance budget, warm cache, ~1k objects: render < 1s, graph + timing < 50ms,
  schema/admission < 3s, online checks < 2s in parallel.
- Configuration: `.fluxlint.yaml` — entrypoints, `externals`, rule severities,
  per-path suppressions with mandatory reason, timing budget, supported Flux version.
- Repo-specific invariants are out of core. A generic **assertions** block evaluates
  CEL over rendered objects and resolved variables (e.g. "an enabled component must
  not have zero replicas"), so users encode their own rollout rules without forking.

## 6. Testing

- Golden fixtures: small synthetic repositories, one per rule, with expected
  findings. No real-world manifests are vendored.
- Differential tests against kustomize-controller's build package to pin rendering
  fidelity per supported Flux version.
- Property tests for the graph package (random DAGs: longest path vs brute force;
  planted cycles are always found; transitive reduction preserves reachability).
- An optional slow conformance suite replays fixtures on a real cluster to confirm
  that each "will not converge" verdict is true. This runs in fluxlint's own CI, not
  in users' pipelines.

## 7. Roadmap

| Milestone | Scope |
| --- | --- |
| **M0** prototype (done, superseded by M1) | shell-out render, event graph, SCC, namespace/CRD/source imports, substitution, max-plus critical path, dependency review |
| **M1** faithful core (done, except SARIF/annotation reporters and differential tests against kustomize-controller) | in-process kustomize, Flux post-processing, config + externals, `FL-G001–5/8`, `FL-S001/2/4`, `FL-T001/2/4–8/100`, text + JSON reports, synthetic fixtures |
| **M2a** external sources (done) | `GitRepository` + `OCIRepository` resolution (source-controller ref precedence, semver ranges), content-addressed cache, `--offline`/`--refresh`, overrides, connect timeout + per-host circuit breaker, `FL-X001/2`; CRDs from external repos join the export index |
| **M2b** Helm + schemas | Helm expansion (`helm template` via SDK, chart cache), CRDs from charts, `FL-V001`, `FL-G006` |
| **M3** runtime wiring | `FL-R*`, Pod Security, webhook analysis, RBAC-as-imports |
| **M4** timing | interval weights, slack, `FL-T*`, observed durations, budget gate, graph export |
| **M5** transitions | `--base` diff analysis, incremental rendering |
| **M6** contracts + assertions | contract format, envtest recipe for operator authors, CEL assertions |
| **1.0** | rule IDs frozen, Flux version support matrix, docs site |

## 8. Known limits

- Controller logic is invisible without a contract.
- Anything produced at runtime must be declared in `externals`; an over-broad
  externals list silently weakens the analysis, so its size is reported.
- Timing output is a bound unless observed durations are supplied.
- Floating refs make results non-reproducible; fluxlint reports them rather than
  pretending otherwise.
