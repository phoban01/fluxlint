// Package policy evaluates the admission policies a repository installs
// against the objects it renders, with the code the API server runs.
package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// Violation is one decision of a bound ValidatingAdmissionPolicy against an
// object.
type Violation struct {
	Policy, Binding string
	// Action is Deny or Warn. Bindings that only audit are not evaluated.
	Action  string
	Message string
	// Undecided is set when the expression could not be evaluated offline (it
	// asks the authorizer, or reads a parameter that is not in Git). With
	// failurePolicy: Fail the API server would reject on an error too, but
	// the error here is fluxlint's, so it is not a verdict.
	Undecided bool
}

type boundPolicy struct {
	policy    *admissionv1.ValidatingAdmissionPolicy
	validator validating.Validator
	bindings  []*admissionv1.ValidatingAdmissionPolicyBinding
}

// Set holds the ValidatingAdmissionPolicies of a rendered tree, compiled.
type Set struct {
	policies   []boundPolicy
	matcher    generic.PolicyMatcher
	namespaces map[string]*corev1.Namespace
	params     map[string][]*unstructured.Unstructured // "group/Kind"
	interfaces admission.ObjectInterfaces
	// CompileErrors maps a policy name to why its expressions do not compile.
	CompileErrors map[string]error
}

type objectInterfaces struct {
	admission.ObjectInterfaces
	mapper runtime.EquivalentResourceMapper
}

func (o objectInterfaces) GetEquivalentResourceMapper() runtime.EquivalentResourceMapper {
	return o.mapper
}

func into(o map[string]any, out any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(o, out)
}

// NewSet compiles every ValidatingAdmissionPolicy among objs that a binding
// among objs enforces. Namespaces among objs answer namespaceSelector and the
// namespaceObject variable; any other object can serve as a parameter.
func NewSet(objs []map[string]any) *Set {
	s := &Set{namespaces: map[string]*corev1.Namespace{}, params: map[string][]*unstructured.Unstructured{}, CompileErrors: map[string]error{}}
	for _, name := range []string{"default", "kube-system", "kube-public", "kube-node-lease"} {
		s.namespaces[name] = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name}}}
	}
	policies := map[string]*admissionv1.ValidatingAdmissionPolicy{}
	var bindings []*admissionv1.ValidatingAdmissionPolicyBinding
	for _, o := range objs {
		u := &unstructured.Unstructured{Object: o}
		gvk := u.GroupVersionKind()
		switch {
		case gvk.Group == "" && gvk.Kind == "Namespace":
			ns := &corev1.Namespace{}
			if into(o, ns) == nil {
				if ns.Labels == nil {
					ns.Labels = map[string]string{}
				}
				ns.Labels["kubernetes.io/metadata.name"] = ns.Name
				s.namespaces[ns.Name] = ns
			}
		case gvk.Group == "admissionregistration.k8s.io" && gvk.Kind == "ValidatingAdmissionPolicy":
			p := &admissionv1.ValidatingAdmissionPolicy{}
			if into(o, p) == nil {
				if p.Spec.MatchConstraints == nil {
					p.Spec.MatchConstraints = &admissionv1.MatchResources{}
				}
				defaultMatch(p.Spec.MatchConstraints)
				if p.Spec.FailurePolicy == nil {
					fail := admissionv1.Fail
					p.Spec.FailurePolicy = &fail
				}
				policies[p.Name] = p
			}
		case gvk.Group == "admissionregistration.k8s.io" && gvk.Kind == "ValidatingAdmissionPolicyBinding":
			b := &admissionv1.ValidatingAdmissionPolicyBinding{}
			if into(o, b) == nil {
				if b.Spec.MatchResources != nil {
					defaultMatch(b.Spec.MatchResources)
				}
				bindings = append(bindings, b)
			}
		}
		key := gvk.Group + "/" + gvk.Kind
		s.params[key] = append(s.params[key], u)
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, ns := range s.namespaces {
		_ = indexer.Add(ns)
	}
	s.matcher = generic.NewPolicyMatcher(matching.NewMatcher(listersv1.NewNamespaceLister(indexer), fake.NewSimpleClientset()))
	s.interfaces = objectInterfaces{admission.NewObjectInterfacesFromScheme(runtime.NewScheme()), runtime.NewEquivalentResourceRegistry()}

	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		bp := boundPolicy{policy: policies[name]}
		for _, b := range bindings {
			if b.Spec.PolicyName == name && (enforces(b, admissionv1.Deny) || enforces(b, admissionv1.Warn)) {
				bp.bindings = append(bp.bindings, b)
			}
		}
		if len(bp.bindings) == 0 {
			continue // a policy without a binding does nothing
		}
		var err error
		bp.validator, err = compile(bp.policy)
		if err != nil {
			s.CompileErrors[name] = err
			continue
		}
		s.policies = append(s.policies, bp)
	}
	return s
}

func enforces(b *admissionv1.ValidatingAdmissionPolicyBinding, action admissionv1.ValidationAction) bool {
	for _, a := range b.Spec.ValidationActions {
		if a == action {
			return true
		}
	}
	return false
}

// Empty reports whether there is nothing to evaluate.
func (s *Set) Empty() bool { return len(s.policies) == 0 }

// compile follows compilePolicy in k8s.io/apiserver's validating plugin,
// which is not exported; every piece it is made of is.
func compile(p *admissionv1.ValidatingAdmissionPolicy) (validating.Validator, error) {
	hasParam := p.Spec.ParamKind != nil
	optional := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true}
	forMessages := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false}
	compiler, err := cel.NewCompositedCompiler(baseEnv())
	if err != nil {
		return nil, err
	}
	variables := make([]cel.NamedExpressionAccessor, len(p.Spec.Variables))
	for i, v := range p.Spec.Variables {
		variables[i] = &validating.Variable{Name: v.Name, Expression: v.Expression}
	}
	compiler.CompileAndStoreVariables(variables, optional, environment.StoredExpressions)

	var matcher matchconditions.Matcher
	if mc := p.Spec.MatchConditions; len(mc) > 0 {
		accessors := make([]cel.ExpressionAccessor, len(mc))
		for i := range mc {
			accessors[i] = (*matchconditions.MatchCondition)(&mc[i])
		}
		matcher = matchconditions.NewMatcher(compiler.CompileCondition(accessors, optional, environment.StoredExpressions), p.Spec.FailurePolicy, "policy", "validate", p.Name)
	}
	validations := make([]cel.ExpressionAccessor, len(p.Spec.Validations))
	messages := make([]cel.ExpressionAccessor, len(p.Spec.Validations))
	for i, v := range p.Spec.Validations {
		validations[i] = &validating.ValidationCondition{Expression: v.Expression, Message: v.Message, Reason: v.Reason}
		if v.MessageExpression != "" {
			messages[i] = &validating.MessageExpressionCondition{MessageExpression: v.MessageExpression}
		}
	}
	conditions := compiler.CompileCondition(validations, optional, environment.StoredExpressions)
	if errs := conditions.CompilationErrors(); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return validating.NewValidator(
		conditions,
		matcher,
		compiler.CompileCondition(nil, optional, environment.StoredExpressions),
		compiler.CompileCondition(messages, forMessages, environment.StoredExpressions),
		p.Spec.FailurePolicy, nil), nil
}

// Request is one apply, as the API server would see it.
type Request struct {
	Object map[string]any
	// Old is the object as it is in the cluster, for an update; nil for a
	// create.
	Old map[string]any
	// Resource is the plural the object is served under.
	Resource string
	// User is who applies: for Flux, the controller's ServiceAccount.
	User string
}

// Evaluate runs every bound policy that matches the request.
func (s *Set) Evaluate(ctx context.Context, req Request) []Violation {
	obj := &unstructured.Unstructured{Object: req.Object}
	gvk := obj.GroupVersionKind()
	gvr := gvk.GroupVersion().WithResource(req.Resource)
	op := admission.Create
	var old runtime.Object
	oldVersioned := admission.NewLazyObject(nil)
	if req.Old != nil {
		op = admission.Update
		o := &unstructured.Unstructured{Object: req.Old}
		old, oldVersioned = o, admission.NewLazyObject(o)
	}
	groups := []string{"system:authenticated"}
	if strings.HasPrefix(req.User, "system:serviceaccount:") {
		if parts := strings.Split(req.User, ":"); len(parts) == 4 {
			groups = append(groups, "system:serviceaccounts", "system:serviceaccounts:"+parts[2])
		}
	}
	var options runtime.Object = &metav1.CreateOptions{}
	if op == admission.Update {
		options = &metav1.UpdateOptions{}
	}
	attr := admission.NewAttributesRecord(obj, old, gvk, obj.GetNamespace(), obj.GetName(), gvr, "", op, options, false,
		&user.DefaultInfo{Name: req.User, Groups: groups})
	versioned := &admission.VersionedAttributes{Attributes: attr, VersionedObject: admission.NewLazyObject(obj), VersionedOldObject: oldVersioned, VersionedKind: gvk}

	var out []Violation
	for _, bp := range s.policies {
		matches, matchedResource, _, err := s.matcher.DefinitionMatches(attr, s.interfaces, validating.NewValidatingAdmissionPolicyAccessor(bp.policy))
		if err != nil || !matches {
			continue // an unknown namespace cannot be selected on: say nothing
		}
		for _, b := range bp.bindings {
			if ok, err := s.matcher.BindingMatches(attr, s.interfaces, validating.NewValidatingAdmissionPolicyBindingAccessor(b)); err != nil || !ok {
				continue
			}
			action := "Warn"
			if enforces(b, admissionv1.Deny) {
				action = "Deny"
			}
			params, missing := s.paramsFor(bp.policy, b, obj.GetNamespace())
			if missing != "" {
				out = append(out, Violation{Policy: bp.policy.Name, Binding: b.Name, Action: action, Undecided: true, Message: missing})
				continue
			}
			// the namespaceObject variable is null for cluster-scoped
			// objects, and for a Namespace it is the object itself
			var ns *corev1.Namespace
			if n := obj.GetNamespace(); n != "" {
				ns = s.namespaces[n]
			}
			for _, param := range params {
				res := bp.validator.Validate(ctx, matchedResource, versioned, param, ns, celconfig.RuntimeCELCostBudget, nil)
				for _, d := range res.Decisions {
					switch {
					case d.Action != validating.ActionDeny:
					case d.Evaluation == validating.EvalError:
						out = append(out, Violation{Policy: bp.policy.Name, Binding: b.Name, Action: action, Undecided: true, Message: d.Message})
					default:
						out = append(out, Violation{Policy: bp.policy.Name, Binding: b.Name, Action: action, Message: d.Message})
					}
				}
			}
		}
	}
	return out
}

// paramsFor resolves a binding's paramRef among the rendered objects. A
// policy without paramKind is evaluated once, with no parameter.
func (s *Set) paramsFor(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding, objectNamespace string) ([]runtime.Object, string) {
	if p.Spec.ParamKind == nil {
		return []runtime.Object{nil}, ""
	}
	ref := b.Spec.ParamRef
	if ref == nil {
		return nil, "the policy takes a parameter and the binding has no paramRef"
	}
	gv, _ := schema.ParseGroupVersion(p.Spec.ParamKind.APIVersion)
	namespace := ref.Namespace
	if namespace == "" {
		namespace = objectNamespace
	}
	var found []runtime.Object
	for _, u := range s.params[gv.Group+"/"+p.Spec.ParamKind.Kind] {
		if u.GetNamespace() != "" && u.GetNamespace() != namespace {
			continue
		}
		switch {
		case ref.Name != "":
			if u.GetName() == ref.Name {
				found = append(found, u)
			}
		case ref.Selector != nil:
			sel, err := metav1.LabelSelectorAsSelector(ref.Selector)
			if err == nil && sel.Matches(labelSet(u.GetLabels())) {
				found = append(found, u)
			}
		}
	}
	if len(found) == 0 {
		if ref.ParameterNotFoundAction != nil && *ref.ParameterNotFoundAction == admissionv1.AllowAction {
			return nil, ""
		}
		return nil, fmt.Sprintf("the parameter (%s %s) is not rendered from Git, so the policy was not evaluated", p.Spec.ParamKind.Kind, ref.Name)
	}
	return found, ""
}

type labelSet map[string]string

func (l labelSet) Has(k string) bool   { _, ok := l[k]; return ok }
func (l labelSet) Get(k string) string { return l[k] }
func (l labelSet) Lookup(k string) (string, bool) {
	v, ok := l[k]
	return v, ok
}

// The API server defaults these fields when a policy or binding is created,
// and the matcher relies on them. The defaulting functions themselves live in
// k8s.io/kubernetes, which cannot be imported.
func defaultMatch(m *admissionv1.MatchResources) {
	if m.NamespaceSelector == nil {
		m.NamespaceSelector = &metav1.LabelSelector{}
	}
	if m.ObjectSelector == nil {
		m.ObjectSelector = &metav1.LabelSelector{}
	}
	if m.MatchPolicy == nil {
		equivalent := admissionv1.Equivalent
		m.MatchPolicy = &equivalent
	}
	all := admissionv1.AllScopes
	for _, rules := range [][]admissionv1.NamedRuleWithOperations{m.ResourceRules, m.ExcludeResourceRules} {
		for i := range rules {
			if rules[i].Scope == nil {
				rules[i].Scope = &all
			}
		}
	}
}

var (
	envOnce sync.Once
	env     *environment.EnvSet
)

// baseEnv is expensive to build and the same for every policy.
func baseEnv() *environment.EnvSet {
	envOnce.Do(func() { env = environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()) })
	return env
}
