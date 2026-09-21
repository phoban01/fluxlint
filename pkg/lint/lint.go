// Package lint runs fluxlint's rules over a rendered tree.
package lint

import (
	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Info    Severity = "info"
	Off     Severity = "off"
)

func (s Severity) rank() int {
	switch s {
	case Error:
		return 0
	case Warning:
		return 1
	default:
		return 2
	}
}

// Rule describes one check. IDs are stable.
type Rule struct {
	ID       string
	Name     string
	Severity Severity
	Help     string
}

// Finding is one reported problem or suggestion.
type Finding struct {
	Rule       string   `json:"rule"`
	Name       string   `json:"name"`
	Severity   Severity `json:"severity"`
	Entrypoint string   `json:"entrypoint"`
	Component  string   `json:"component,omitempty"`
	Object     string   `json:"object,omitempty"`
	File       string   `json:"file,omitempty"`
	Line       int      `json:"line,omitempty"`
	Message    string   `json:"message"`
	Detail     []string `json:"detail,omitempty"`
	// Existing is set by Compare when the same finding is present at the base
	// commit: it does not fail a --base run.
	Existing bool `json:"existing,omitempty"`
}

// Rules is the catalogue, in report order.
var Rules = []Rule{
	{"FL-G001", "dangling-reference", Error, "A dependsOn or sourceRef names an object that nothing in the repository defines. Flux reports the dependent as not ready forever."},
	{"FL-G002", "bootstrap-deadlock", Error, "The precedence graph has a cycle, so the repository cannot converge from an empty cluster. Each edge of the cycle is listed with its reason; remove or reverse one."},
	{"FL-G003", "dual-ownership", Error, "The same object is rendered by more than one Flux Kustomization. They overwrite each other on every reconcile, and pruning one deletes the object for both."},
	{"FL-G004", "missing-namespace", Error, "An object is applied into a namespace that nothing creates. Declare it under externals.namespaces if it is created out of band."},
	{"FL-G005", "unknown-crd", Warning, "Custom resources of this API group are applied but no rendered component installs its CRDs. They may come from a Helm chart or external source fluxlint has not rendered; declare the group under externals.crdGroups to acknowledge."},
	{"FL-G008", "build-failed", Error, "kustomize build (or post-build substitution) failed for the path a Flux Kustomization points at."},
	{"FL-S001", "undefined-variable", Error, "A ${var} has no definition in postBuild.substitute / substituteFrom and no default. Flux substitutes the empty string."},
	{"FL-S002", "missing-substitute-source", Error, "postBuild.substituteFrom names a ConfigMap/Secret that nothing renders and that is not optional. Declare it under externals.substitutions if it is created out of band."},
	{"FL-S004", "unexpanded-variable", Info, "${...} expressions appear in a Kustomization without postBuild, so they reach the cluster verbatim. Expected for shell snippets; a bug if Flux substitution was intended."},
	{"FL-G006", "unserved-version", Error, "A custom resource uses an apiVersion that the CRD rendered for it does not serve. The apply fails with 'no matches for kind'."},
	{"FL-V001", "schema-violation", Error, "A custom resource fails the OpenAPI schema of the CRD rendered for it, after defaulting — the same validation the API server performs. Unknown fields are not reported: a structural schema prunes them."},
	{"FL-V003", "invalid-builtin", Error, "An object of a built-in API group is not valid for the Kubernetes release the cluster runs (kubeVersion, which can differ per entrypoint): a field that does not exist there, a value of the wrong type, a required field that is missing, or an API version that release does not serve. Every Kubernetes release publishes the OpenAPI schema of its APIs; fluxlint fetches the one for your release once and caches it, so a field added in 1.36 is an error on a 1.35 cluster and fine on a 1.36 one. Set kubeVersion to the release you plan to upgrade to and the same check says what will break. Flux applies with server-side apply, which rejects all of these. When the schemas cannot be loaded (offline with a cold cache), the Kubernetes types bundled with fluxlint are used and the run says so."},
	{"FL-V004", "invalid-value", Error, "A built-in object has the right fields and types but a value the API server rejects: a selector that does not match the pod template, a port out of range or a port name over 15 characters, a volumeMount with no volume, a CronJob schedule that does not parse, a Service with two unnamed ports. Flux applies with a server-side dry run first, so one such object fails its whole Kustomization. The checks restate the API server's own validation and cover the common workload, Service, Ingress, RBAC and storage kinds; an object of another kind is not checked."},
	{"FL-V002", "pod-security", Error, "A pod template violates the Pod Security level its namespace enforces (pod-security.kubernetes.io/enforce), evaluated with the API server's own checks. The workload is accepted but its pods are never created."},
	{"FL-V005", "policy-violation", Error, "A ValidatingAdmissionPolicy that the repository installs, bound with Deny, rejects an object that the repository renders. The policy is compiled and evaluated with the API server's own code: matchConstraints, matchConditions, variables, parameters that are in Git, namespaceObject, and request.userInfo set to the Flux controller's ServiceAccount. Every object is evaluated as a create. With --base, every object the change modifies is also evaluated as an update with oldObject, which is where immutability rules speak. A binding that only warns gives a warning; one that only audits is skipped. An expression that needs the authorizer, or a parameter that is not in Git, is listed as not evaluated."},
	{"FL-V006", "policy-invalid", Error, "A bound ValidatingAdmissionPolicy has an expression that does not compile. With failurePolicy: Fail, which is the default, the API server then rejects every request the policy matches."},
	{"FL-V007", "kyverno-violation", Error, "A Kyverno policy that the repository installs, in Enforce mode, rejects an object that the repository renders. Kyverno's engine cannot be linked into another program, so the policies are evaluated by the kyverno CLI, which is that engine: install the version your clusters run and the verdict is the one they would give. Nothing is downloaded or started; when the CLI is not on PATH the policies are listed as not evaluated. Failures of Audit policies, and rules that need the cluster (apiCall, configMap context), are suggestions. Policy exceptions in the repository are honoured. Policies that mutate or generate are not applied."},
	{"FL-R001", "unresolved-config-reference", Error, "A pod references a Secret or ConfigMap (or a key of one) that nothing creates: not a manifest, an ExternalSecret, a ClusterExternalSecret selecting the namespace, a cert-manager Certificate, nor externals.secrets. The pod stays in ContainerCreating / CreateContainerConfigError."},
	{"FL-R002", "unresolved-identity-reference", Error, "A pod names a ServiceAccount or imagePullSecret that nothing creates in its namespace. Pods are not created, or cannot pull their image."},
	{"FL-R003", "positional-patch", Warning, "A JSON patch in a Flux Kustomization addresses a list element by index (env/3, containers/0/args/2). When the upstream manifest adds or reorders entries — typically on a version bump — the patch silently applies to a different element. Prefer a strategic-merge patch keyed by name."},
	{"FL-R004", "webhook-backend", Error, "An admission webhook's Service does not exist, selects no pods, or fronts only workloads scaled to zero while the webhook fails closed."},
	{"FL-A001", "assertion-failed", Error, "A repository-specific assertion from .fluxlint.yaml does not hold for a rendered object."},
	{"FL-A002", "assertion-invalid", Error, "An assertion in .fluxlint.yaml does not compile or does not evaluate to a bool."},
	{"FL-D001", "prune", Info, "Objects present at the base commit are gone, and their Kustomization prunes: Flux will delete them. Raised to a warning when namespaces, CRDs, PVCs, StatefulSets or Secrets are among them."},
	{"FL-D002", "immutable-change", Error, "The change updates a field the API server treats as immutable (a workload selector, a StatefulSet's volumeClaimTemplates, a Job template, a RoleBinding's roleRef …). The apply is rejected unless the Kustomization sets force: true, which deletes and recreates the object."},
	{"FL-D003", "ownership-move", Warning, "An object moves from one Flux Kustomization to another. If the old owner prunes and reconciles after the new owner has applied, the object is deleted and only comes back on the next reconcile."},
	{"FL-D004", "orphaned", Info, "Objects leave Git but their Kustomization has prune: false, so they remain in the cluster with nothing managing them."},
	{"FL-R007", "module-skew", Warning, "A controller rendered from a Git source is built against a newer version of an API module than the tag pinned for the component that installs that module's CRDs. Read from go.mod in both sources."},
	{"FL-R008", "admission-window", Warning, "A component applies objects that a fail-closed admission webhook intercepts, the webhook is installed by another component, and nothing makes the apply wait until the webhook answers. Between the webhook being registered and its pods serving, the API server rejects every matching request, so the apply fails and is retried a retryInterval later. dependsOn alone is not enough: a Kustomization without wait: true is Ready as soon as it is applied. Webhook rules, objectSelector and namespaceSelector are evaluated, and the webhooks Kyverno registers at runtime are worked out from its chart and its policies. Two cases are suggestions rather than warnings: a component applied before the webhook exists (its first apply is safe, a retry or the next interval is not), and a selector that needs a label which is not set in Git (controllers often set such labels at runtime). A webhook with matchConditions is not evaluated."},
	{"FL-R009", "contested-field", Warning, "The manifests set a field that another controller also writes: spec.replicas on a workload that a HorizontalPodAutoscaler or KEDA ScaledObject scales, or a caBundle that cert-manager is asked to inject. Flux takes the field back on every reconcile and the other controller changes it again. The first install looks healthy; the fight shows on the second reconcile."},
	{"FL-R010", "unstable-render", Info, "A Helm chart renders an object differently each time: a template calls randAlphaNum, genCA, now or a similar function. Helm renders again on every upgrade, so the object changes whenever the release is upgraded, whatever the upgrade was for: a generated password is replaced under a running database, a certificate is reissued, pods restart. Raised to a warning for Secrets and webhook configurations. A template that also calls lookup is not reported, because it usually keeps the value already in the cluster."},
	{"FL-R011", "contested-secret", Error, "Two ExternalSecrets create the same Secret in the same namespace and both want to own it, directly or through ClusterExternalSecrets whose namespaces overlap. The operator refuses the second with \"already owned by another ExternalSecret\"; it never becomes Ready, and neither does its ClusterExternalSecret. Namespaces are taken from spec.namespaces and from selectors evaluated against the namespaces in Git. target.creationPolicy Merge, Orphan and None do not take ownership and are not reported."},
	{"FL-R012", "missing-issuer", Error, "A cert-manager Certificate names an Issuer or ClusterIssuer that nothing in the repository creates. The Certificate is accepted and stays pending, the Secret it should produce never appears, and every pod that mounts it waits in ContainerCreating. A warning when some component could not be rendered and may create the issuer. Issuers of other API groups are not checked."},
	{"FL-R013", "single-pod-gate", Warning, "A fail-closed admission webhook that admits the applies of other components is served by exactly one pod. No ordering helps: whenever that pod restarts, is rescheduled or is upgraded, every matching apply in the cluster is rejected until it is back, and each rejected Kustomization waits a retryInterval. Webhooks that Kyverno registers at runtime are included, worked out from its chart and its policies. A warning when three or more other components go through the webhook, a suggestion below that. Workloads under an autoscaler, and DaemonSets, are taken to have more than one pod."},
	{"FL-R014", "source-credentials", Error, "A GitRepository, OCIRepository, HelmRepository or Bucket names a Secret (secretRef, certSecretRef, proxySecretRef) that nothing creates: not a manifest, an ExternalSecret, a ClusterExternalSecret selecting the namespace, nor externals.secrets. The source never becomes Ready and everything that reads from it waits. The repository's own source is left out: flux bootstrap creates its Secret in the cluster. A warning when some component could not be rendered and may create it."},
	{"FL-R015", "values-reference", Error, "A HelmRelease's valuesFrom names a Secret or ConfigMap, or a key of one (valuesKey, default values.yaml), that nothing creates. helm-controller cannot compose the values (\"could not resolve Secret chart values reference … key not found\"), so the release is never installed. Producers are manifests, ExternalSecrets, ClusterExternalSecrets selecting the namespace, Certificates and externals.secrets; one that does not list its keys (an ExternalSecret with dataFrom) is taken to have any key. Entries marked optional are skipped."},
	{"FL-O001", "unpredicted-failure", Error, "With --cluster-state: a Kustomization or HelmRelease is not Ready in a real cluster, and no error or warning named it. This is a gap in what fluxlint models. Failures that only repeat another (a dependency or a child that is itself not Ready) are not reported. Read the cluster's message, fix the cause, and consider an assertion or a contract so that the next occurrence is found before the cluster is."},
	{"FL-O002", "unconfirmed-finding", Info, "With --cluster-state: fluxlint reports an error on a component that is Ready in a real cluster. Either the finding is a false positive, or the cluster was helped by something outside Git, such as a test script that seeds a Secret."},
	{"FL-O003", "not-compared", Info, "With --cluster-state: components that exist on one side only, so there was nothing to compare. Suspended objects are left out."},
	{"FL-C001", "contract-unmet", Error, "A component ships a fluxlint-contract.yaml declaring what it cannot run without — CRDs it watches, Secret and ConfigMap keys it reads — and this repository does not provide it, or provides it without ordering."},
	{"FL-X001", "source-unavailable", Warning, "A Kustomization reads from another repository or artifact that could not be materialised, so nothing it applies was analysed. Run without --offline, fix access, or map it with sources.overrides. Raised to an error when the host answered and the ref, tag or chart version is not there: Flux will not find it either, and everything that waits for the component times out."},
	{"FL-X002", "floating-ref", Info, "A source follows a branch or semver range. What Flux applies can change without a commit to this repository, and fluxlint's result reflects whatever was fetched last."},
	{"FL-X003", "render-gap", Info, "Part of a component's spec is not modelled, so what fluxlint analysed may differ from what the controller applies."},
	{"FL-X004", "image-not-found", Error, "A pod template names a container image that its registry does not have: a tag that was never pushed, a typo, a Git tag mistaken for an image tag. With images.platforms set, also an image that is not built for your nodes. The pod stays in ImagePullBackOff, and a Kustomization that waits for it times out. Only images matching images.verify are looked up, with the credentials of your Docker config; an image that could not be checked is a render note, not a finding. Scaled-to-zero workloads are reported as a warning: nothing pulls the image until they are scaled up."},
	{"FL-T001", "critical-path", Info, "The chain of waits that determines the worst-case time for a bootstrap to converge. A bound, not a prediction."},
	{"FL-T002", "dominant-delay", Info, "One component contributes a large share of the worst-case bound, usually a generous timeout underneath a wait: true parent."},
	{"FL-T004", "unjustified-dependency", Info, "A dependsOn edge for which no import exists between the two subtrees. It may encode a runtime need fluxlint cannot see; if not, it only serialises reconciliation."},
	{"FL-T005", "redundant-dependency", Info, "A dependsOn edge already implied by another dependsOn path."},
	{"FL-T006", "implicit-ordering", Warning, "A component imports something from another without any declared ordering. It converges only by failing and retrying."},
	{"FL-T007", "retry-cliff", Warning, "No retryInterval and a long interval: one failed apply stalls the component for a full interval."},
	{"FL-T008", "timeout-inversion", Warning, "A wait: true parent times out before the components it waits for are allowed to, so it flaps NotReady while they legitimately converge."},
	{"FL-T010", "stale-substitution", Warning, "A Kustomization substitutes variables from a ConfigMap or Secret that another Kustomization of the same source applies, with no dependsOn between them. Both reconcile when a commit arrives; if the reader goes first it uses the old values and is not triggered again until its interval elapses. With a dependsOn, kustomize-controller waits until the dependency has applied the same revision."},
	{"FL-T100", "bootstrap-budget", Error, "The worst-case bootstrap bound exceeds timing.maxBootstrapBound."},
}

// RuleByID looks a rule up by ID or name.
func RuleByID(id string) (Rule, bool) {
	for _, r := range Rules {
		if r.ID == id || r.Name == id {
			return r, true
		}
	}
	return Rule{}, false
}

type run struct {
	files    map[string][]string // repository file -> lines, for locations
	ix       *Index
	cfg      *config.Config
	findings []Finding
}

func (r *run) report(id string, c *model.Component, o model.Object, msg string, detail ...string) {
	rule, ok := RuleByID(id)
	if !ok {
		panic("lint: no rule " + id + " in the catalogue")
	}
	r.reportAs(rule.Severity, id, c, o, msg, detail...)
}

// reportAs reports with a severity other than the rule's default; an explicit
// override in the configuration still wins.
func (r *run) reportAs(sev Severity, id string, c *model.Component, o model.Object, msg string, detail ...string) {
	rule, _ := RuleByID(id)
	if r.cfg.Rules.Off[id] {
		return
	}
	if s, ok := r.cfg.Rules.Level[id]; ok {
		sev = Severity(s)
	}
	if sev == Off {
		return
	}
	f := Finding{Rule: id, Name: rule.Name, Severity: sev, Entrypoint: r.ix.Tree.Entrypoint, Message: msg, Detail: detail}
	if c != nil {
		f.Component = c.String()
	}
	if o != nil {
		f.Object = o.String()
	}
	f.File, f.Line = r.locate(c, o)
	r.findings = append(r.findings, f)
}

// Result of analysing one entrypoint.
type Result struct {
	Tree     *model.Tree
	Findings []Finding
	Timing   *Timing
}

// Run executes every rule.
func Run(t *model.Tree, cfg *config.Config) *Result {
	clusters := clusterIndexes(t, cfg)
	whole := mergeIndexes(t, cfg, clusters)
	r := &run{ix: whole, cfg: cfg, files: map[string][]string{}}
	r.graphRules()
	r.substitutionRules()
	declared := declaredGraph(whole)
	// everything that matches objects by name, within one cluster at a time
	for _, ix := range clusters {
		r.ix = ix
		r.runtimeRules()
		r.admissionWindows(declared)
		r.contestedFields()
		r.secretOwners()
		r.certificateIssuers()
		r.sourceCredentials()
		r.valuesReferences()
		r.admissionRules()
		r.controllerRules()
	}
	r.ix = whole
	r.unstableRenders()
	r.imageRules()
	r.assertionRules()
	timing := r.timingRules()
	r.clusterRules()
	sortFindings(r.findings)
	return &Result{Tree: t, Findings: r.findings, Timing: timing}
}
