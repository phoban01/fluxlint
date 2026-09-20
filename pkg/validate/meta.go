package validate

import (
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// nameRule says how the API server validates metadata.name for a kind. Kinds
// that are not listed are left alone.
func nameRule(obj runtime.Object) (check func(string) []string, known bool) {
	switch obj.(type) {
	case *corev1.Namespace:
		return validation.IsDNS1123Label, true
	case *corev1.Service:
		return validation.IsDNS1035Label, true
	case *rbacv1.Role, *rbacv1.ClusterRole, *rbacv1.RoleBinding, *rbacv1.ClusterRoleBinding:
		// RBAC names may contain colons: they only have to be a path segment
		return func(name string) []string { return pathSegment(name) }, true
	case *corev1.Pod, *corev1.ConfigMap, *corev1.Secret, *corev1.ServiceAccount, *corev1.PersistentVolumeClaim,
		*appsv1.Deployment, *appsv1.StatefulSet, *appsv1.DaemonSet, *appsv1.ReplicaSet,
		*batchv1.Job, *batchv1.CronJob, *networkingv1.Ingress, *networkingv1.NetworkPolicy,
		*autoscalingv2.HorizontalPodAutoscaler, *policyv1.PodDisruptionBudget:
		return validation.IsDNS1123Subdomain, true
	}
	return nil, false
}

func pathSegment(name string) []string {
	var msgs []string
	if name == "." || name == ".." {
		msgs = append(msgs, `may not be "." or ".."`)
	}
	for _, illegal := range []string{"/", "%"} {
		if strings.Contains(name, illegal) {
			msgs = append(msgs, `may not contain "`+illegal+`"`)
		}
	}
	return msgs
}

func objectMeta(obj runtime.Object) field.ErrorList {
	var errs field.ErrorList
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil
	}
	path := field.NewPath("metadata")
	if check, known := nameRule(obj); known {
		switch {
		case m.GetName() == "" && m.GetGenerateName() == "":
			errs = append(errs, field.Required(path.Child("name"), "name or generateName is required"))
		case m.GetName() != "":
			for _, msg := range check(m.GetName()) {
				errs = append(errs, field.Invalid(path.Child("name"), m.GetName(), msg))
			}
			// a CronJob's name is a prefix of its Jobs' names, which are prefixes of Pod names
			if _, ok := obj.(*batchv1.CronJob); ok && len(m.GetName()) > 52 {
				errs = append(errs, field.Invalid(path.Child("name"), m.GetName(), "must be no more than 52 characters"))
			}
		}
	}
	if ns := m.GetNamespace(); ns != "" {
		for _, msg := range validation.IsDNS1123Label(ns) {
			errs = append(errs, field.Invalid(path.Child("namespace"), ns, msg))
		}
	}
	errs = append(errs, metav1validation.ValidateLabels(m.GetLabels(), path.Child("labels"))...)
	errs = append(errs, apivalidation.ValidateAnnotations(m.GetAnnotations(), path.Child("annotations"))...)
	return errs
}
