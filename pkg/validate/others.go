package validate

import (
	"fmt"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func service(s *corev1.Service) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	switch s.Spec.Type {
	case "", corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		if len(s.Spec.Ports) == 0 && s.Spec.ClusterIP != corev1.ClusterIPNone {
			errs = append(errs, field.Required(spec.Child("ports"), ""))
		}
	case corev1.ServiceTypeExternalName:
		if s.Spec.ExternalName == "" {
			errs = append(errs, field.Required(spec.Child("externalName"), ""))
		}
	default:
		errs = append(errs, field.NotSupported(spec.Child("type"), string(s.Spec.Type), []string{"ClusterIP", "ExternalName", "LoadBalancer", "NodePort"}))
	}

	names, seen := map[string]bool{}, map[string]bool{}
	for i, p := range s.Spec.Ports {
		pp := spec.Child("ports").Index(i)
		switch {
		case p.Name == "" && len(s.Spec.Ports) > 1:
			errs = append(errs, field.Required(pp.Child("name"), "when there is more than one port"))
		case p.Name != "":
			for _, msg := range validation.IsDNS1123Label(p.Name) {
				errs = append(errs, field.Invalid(pp.Child("name"), p.Name, msg))
			}
			if names[p.Name] {
				errs = append(errs, field.Duplicate(pp.Child("name"), p.Name))
			}
			names[p.Name] = true
		}
		errs = append(errs, portNumber(int(p.Port), pp.Child("port"))...)
		errs = append(errs, protocol(p.Protocol, pp.Child("protocol"))...)
		errs = append(errs, portOrName(p.TargetPort, pp.Child("targetPort"))...)
		if p.NodePort != 0 {
			errs = append(errs, portNumber(int(p.NodePort), pp.Child("nodePort"))...)
			if s.Spec.Type == "" || s.Spec.Type == corev1.ServiceTypeClusterIP {
				errs = append(errs, field.Forbidden(pp.Child("nodePort"), "may not be used when `type` is 'ClusterIP'"))
			}
		}
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		key := fmt.Sprintf("%s/%d", proto, p.Port)
		if seen[key] {
			errs = append(errs, field.Duplicate(pp, key))
		}
		seen[key] = true
	}
	for k, v := range s.Spec.Selector {
		for _, msg := range validation.IsQualifiedName(k) {
			errs = append(errs, field.Invalid(spec.Child("selector"), k, msg))
		}
		for _, msg := range validation.IsValidLabelValue(v) {
			errs = append(errs, field.Invalid(spec.Child("selector"), v, msg))
		}
	}
	switch s.Spec.SessionAffinity {
	case "", corev1.ServiceAffinityNone, corev1.ServiceAffinityClientIP:
	default:
		errs = append(errs, field.NotSupported(spec.Child("sessionAffinity"), string(s.Spec.SessionAffinity), []string{"ClientIP", "None"}))
	}
	return errs
}

// portOrName validates a target port: a number in range, or the name of a
// container port.
func portOrName(v intstr.IntOrString, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	switch {
	case v.Type == intstr.Int && v.IntVal != 0:
		errs = append(errs, portNumber(int(v.IntVal), path)...)
	case v.Type == intstr.String:
		for _, msg := range validation.IsValidPortName(v.StrVal) {
			errs = append(errs, field.Invalid(path, v.StrVal, msg))
		}
	}
	return errs
}

func configMap(cm *corev1.ConfigMap) field.ErrorList {
	var errs field.ErrorList
	for k := range cm.Data {
		for _, msg := range validation.IsConfigMapKey(k) {
			errs = append(errs, field.Invalid(field.NewPath("data").Key(k), k, msg))
		}
		if _, both := cm.BinaryData[k]; both {
			errs = append(errs, field.Invalid(field.NewPath("data").Key(k), k, "duplicate of key present in binaryData"))
		}
	}
	for k := range cm.BinaryData {
		for _, msg := range validation.IsConfigMapKey(k) {
			errs = append(errs, field.Invalid(field.NewPath("binaryData").Key(k), k, msg))
		}
	}
	return errs
}

func secret(s *corev1.Secret) field.ErrorList {
	var errs field.ErrorList
	for k := range s.Data {
		for _, msg := range validation.IsConfigMapKey(k) {
			errs = append(errs, field.Invalid(field.NewPath("data").Key(k), k, msg))
		}
	}
	for k := range s.StringData {
		for _, msg := range validation.IsConfigMapKey(k) {
			errs = append(errs, field.Invalid(field.NewPath("stringData").Key(k), k, msg))
		}
	}
	has := func(k string) bool {
		_, d := s.Data[k]
		_, sd := s.StringData[k]
		return d || sd
	}
	required := map[corev1.SecretType][]string{
		corev1.SecretTypeDockerConfigJson: {corev1.DockerConfigJsonKey},
		corev1.SecretTypeDockercfg:        {corev1.DockerConfigKey},
		corev1.SecretTypeTLS:              {corev1.TLSCertKey, corev1.TLSPrivateKeyKey},
		corev1.SecretTypeSSHAuth:          {corev1.SSHAuthPrivateKey},
	}
	for _, k := range required[s.Type] {
		if !has(k) {
			errs = append(errs, field.Required(field.NewPath("data").Key(k), "for a Secret of type "+string(s.Type)))
		}
	}
	return errs
}

func claimSpec(spec *corev1.PersistentVolumeClaimSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(spec.AccessModes) == 0 {
		errs = append(errs, field.Required(path.Child("accessModes"), "at least 1 access mode is required"))
	}
	for i, m := range spec.AccessModes {
		switch m {
		case corev1.ReadWriteOnce, corev1.ReadOnlyMany, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
		default:
			errs = append(errs, field.NotSupported(path.Child("accessModes").Index(i), string(m), []string{"ReadOnlyMany", "ReadWriteMany", "ReadWriteOnce", "ReadWriteOncePod"}))
		}
	}
	storage, ok := spec.Resources.Requests[corev1.ResourceStorage]
	switch {
	case !ok:
		errs = append(errs, field.Required(path.Child("resources", "requests").Key("storage"), ""))
	case storage.Sign() <= 0:
		errs = append(errs, field.Invalid(path.Child("resources", "requests").Key("storage"), storage.String(), "must be greater than zero"))
	}
	return errs
}

func claimTemplates(claims []corev1.PersistentVolumeClaim, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i := range claims {
		p := path.Index(i)
		if claims[i].Name == "" {
			errs = append(errs, field.Required(p.Child("metadata", "name"), ""))
		}
		errs = append(errs, claimSpec(&claims[i].Spec, p.Child("spec"))...)
	}
	return errs
}

func ingress(ing *networkingv1.Ingress) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if ing.Spec.DefaultBackend == nil && len(ing.Spec.Rules) == 0 {
		errs = append(errs, field.Invalid(spec, "", "either `defaultBackend` or `rules` must be specified"))
	}
	backend := func(b *networkingv1.IngressBackend, p *field.Path) {
		switch {
		case b.Service != nil && b.Resource != nil:
			errs = append(errs, field.Invalid(p, "", "cannot set both resource and service backends"))
		case b.Service == nil && b.Resource == nil:
			errs = append(errs, field.Required(p, "must specify a service or resource backend"))
		case b.Service != nil:
			if b.Service.Name == "" {
				errs = append(errs, field.Required(p.Child("service", "name"), ""))
			}
			port := b.Service.Port
			switch {
			case port.Name != "" && port.Number != 0:
				errs = append(errs, field.Invalid(p.Child("service", "port"), "", "cannot set both port name & port number"))
			case port.Name == "" && port.Number == 0:
				errs = append(errs, field.Required(p.Child("service", "port"), "port name or number is required"))
			case port.Number != 0:
				errs = append(errs, portNumber(int(port.Number), p.Child("service", "port", "number"))...)
			}
		}
	}
	if ing.Spec.DefaultBackend != nil {
		backend(ing.Spec.DefaultBackend, spec.Child("defaultBackend"))
	}
	for i, rule := range ing.Spec.Rules {
		rp := spec.Child("rules").Index(i)
		if host := rule.Host; host != "" {
			check := validation.IsDNS1123Subdomain
			if strings.HasPrefix(host, "*.") {
				check = validation.IsWildcardDNS1123Subdomain
			}
			for _, msg := range check(host) {
				errs = append(errs, field.Invalid(rp.Child("host"), host, msg))
			}
		}
		if rule.HTTP == nil {
			continue
		}
		if len(rule.HTTP.Paths) == 0 {
			errs = append(errs, field.Required(rp.Child("http", "paths"), ""))
		}
		for j, path := range rule.HTTP.Paths {
			pp := rp.Child("http", "paths").Index(j)
			switch {
			case path.PathType == nil:
				errs = append(errs, field.Required(pp.Child("pathType"), "pathType must be specified"))
			case *path.PathType == networkingv1.PathTypeExact || *path.PathType == networkingv1.PathTypePrefix:
				if !strings.HasPrefix(path.Path, "/") {
					errs = append(errs, field.Invalid(pp.Child("path"), path.Path, "must be an absolute path"))
				}
			case *path.PathType == networkingv1.PathTypeImplementationSpecific:
			default:
				errs = append(errs, field.NotSupported(pp.Child("pathType"), string(*path.PathType), []string{"Exact", "ImplementationSpecific", "Prefix"}))
			}
			backend(&path.Backend, pp.Child("backend"))
		}
	}
	return errs
}

func autoscaler(h *autoscalingv2.HorizontalPodAutoscaler) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if h.Spec.MaxReplicas < 1 {
		errs = append(errs, field.Invalid(spec.Child("maxReplicas"), h.Spec.MaxReplicas, "must be greater than 0"))
	}
	if m := h.Spec.MinReplicas; m != nil {
		switch {
		case *m < 0:
			errs = append(errs, field.Invalid(spec.Child("minReplicas"), *m, "must be greater than or equal to 0"))
		case h.Spec.MaxReplicas >= 1 && *m > h.Spec.MaxReplicas:
			errs = append(errs, field.Invalid(spec.Child("maxReplicas"), h.Spec.MaxReplicas, "must be greater than or equal to `minReplicas`"))
		}
	}
	ref := h.Spec.ScaleTargetRef
	if ref.Kind == "" {
		errs = append(errs, field.Required(spec.Child("scaleTargetRef", "kind"), ""))
	}
	if ref.Name == "" {
		errs = append(errs, field.Required(spec.Child("scaleTargetRef", "name"), ""))
	}
	return errs
}

func disruptionBudget(p *policyv1.PodDisruptionBudget) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if p.Spec.MinAvailable != nil && p.Spec.MaxUnavailable != nil {
		errs = append(errs, field.Invalid(spec, "", "minAvailable and maxUnavailable cannot be both set"))
	}
	errs = append(errs, intOrPercent(p.Spec.MinAvailable, spec.Child("minAvailable"))...)
	errs = append(errs, intOrPercent(p.Spec.MaxUnavailable, spec.Child("maxUnavailable"))...)
	return errs
}

func roleRef(ref rbacv1.RoleRef, namespaced bool) field.ErrorList {
	path := field.NewPath("roleRef")
	var errs field.ErrorList
	if ref.APIGroup != rbacv1.GroupName {
		errs = append(errs, field.NotSupported(path.Child("apiGroup"), ref.APIGroup, []string{rbacv1.GroupName}))
	}
	kinds := []string{"ClusterRole"}
	if namespaced {
		kinds = []string{"ClusterRole", "Role"}
	}
	ok := false
	for _, k := range kinds {
		ok = ok || ref.Kind == k
	}
	if !ok {
		errs = append(errs, field.NotSupported(path.Child("kind"), ref.Kind, kinds))
	}
	if ref.Name == "" {
		errs = append(errs, field.Required(path.Child("name"), ""))
	}
	return errs
}

func subjects(list []rbacv1.Subject, namespacedBinding bool) field.ErrorList {
	var errs field.ErrorList
	for i, s := range list {
		p := field.NewPath("subjects").Index(i)
		if s.Name == "" {
			errs = append(errs, field.Required(p.Child("name"), ""))
		}
		switch s.Kind {
		case rbacv1.ServiceAccountKind:
			if s.APIGroup != "" {
				errs = append(errs, field.NotSupported(p.Child("apiGroup"), s.APIGroup, []string{""}))
			}
			// a RoleBinding defaults the namespace to its own; a ClusterRoleBinding cannot
			if s.Namespace == "" && !namespacedBinding {
				errs = append(errs, field.Required(p.Child("namespace"), ""))
			}
		case rbacv1.UserKind, rbacv1.GroupKind:
			if s.APIGroup != rbacv1.GroupName && s.APIGroup != "" {
				errs = append(errs, field.NotSupported(p.Child("apiGroup"), s.APIGroup, []string{rbacv1.GroupName}))
			}
		default:
			errs = append(errs, field.NotSupported(p.Child("kind"), s.Kind, []string{"ServiceAccount", "User", "Group"}))
		}
	}
	return errs
}
