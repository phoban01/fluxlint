// Package validate checks the values of built-in Kubernetes objects the way
// the API server does when they are created.
//
// Decoding an object into its Go type proves its fields exist and have the
// right types. It does not prove the API server accepts it: a Deployment whose
// selector does not match its template, a Service port of 70000 and a container
// port named "metrics-endpoint-http" all decode, and all are rejected. Flux
// then reports the whole Kustomization as failed, because it applies with a
// server-side dry run first.
//
// The API server's own validation is not importable, so these checks restate
// it. Each one mirrors a rule in k8s.io/kubernetes/pkg/apis/*/validation and
// uses the helpers Kubernetes publishes in k8s.io/apimachinery. The package is
// deliberately conservative: it contains only rules that reject an object
// outright, never ones that depend on the cluster's configuration or version.
package validate

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// Object returns what the API server would reject about obj. Kinds it knows
// nothing about yield no errors.
func Object(obj runtime.Object) field.ErrorList {
	var errs field.ErrorList
	errs = append(errs, objectMeta(obj)...)
	spec := field.NewPath("spec")
	switch o := obj.(type) {
	case *corev1.Pod:
		errs = append(errs, podSpec(&o.Spec, spec, anyRestartPolicy)...)
	case *appsv1.Deployment:
		errs = append(errs, deployment(o)...)
	case *appsv1.StatefulSet:
		var claims []string
		for _, c := range o.Spec.VolumeClaimTemplates {
			claims = append(claims, c.Name)
		}
		errs = append(errs, replicas(o.Spec.Replicas, spec.Child("replicas"))...)
		errs = append(errs, selectorAndTemplate(o.Spec.Selector, &o.Spec.Template, spec, restartAlways, claims...)...)
		errs = append(errs, claimTemplates(o.Spec.VolumeClaimTemplates, spec.Child("volumeClaimTemplates"))...)
	case *appsv1.DaemonSet:
		errs = append(errs, selectorAndTemplate(o.Spec.Selector, &o.Spec.Template, spec, restartAlways)...)
	case *appsv1.ReplicaSet:
		errs = append(errs, replicas(o.Spec.Replicas, spec.Child("replicas"))...)
		errs = append(errs, selectorAndTemplate(o.Spec.Selector, &o.Spec.Template, spec, restartAlways)...)
	case *batchv1.Job:
		errs = append(errs, jobSpec(&o.Spec, spec)...)
	case *batchv1.CronJob:
		errs = append(errs, cronJob(o)...)
	case *corev1.Service:
		errs = append(errs, service(o)...)
	case *corev1.ConfigMap:
		errs = append(errs, configMap(o)...)
	case *corev1.Secret:
		errs = append(errs, secret(o)...)
	case *corev1.PersistentVolumeClaim:
		errs = append(errs, claimSpec(&o.Spec, spec)...)
	case *networkingv1.Ingress:
		errs = append(errs, ingress(o)...)
	case *autoscalingv2.HorizontalPodAutoscaler:
		errs = append(errs, autoscaler(o)...)
	case *policyv1.PodDisruptionBudget:
		errs = append(errs, disruptionBudget(o)...)
	case *rbacv1.RoleBinding:
		errs = append(errs, roleRef(o.RoleRef, true)...)
		errs = append(errs, subjects(o.Subjects, true)...)
	case *rbacv1.ClusterRoleBinding:
		errs = append(errs, roleRef(o.RoleRef, false)...)
		errs = append(errs, subjects(o.Subjects, false)...)
	}
	return errs
}
