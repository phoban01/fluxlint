package validate

import (
	"strings"

	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func replicas(n *int32, path *field.Path) field.ErrorList {
	if n != nil && *n < 0 {
		return field.ErrorList{field.Invalid(path, *n, "must be greater than or equal to 0")}
	}
	return nil
}

// selectorAndTemplate is the rule behind the most common rejected workload: the
// selector is required, may not be empty, and must select the pods the
// template creates.
func selectorAndTemplate(sel *metav1.LabelSelector, tmpl *corev1.PodTemplateSpec, spec *field.Path, restart restartRule, claims ...string) field.ErrorList {
	var errs field.ErrorList
	switch {
	case sel == nil:
		errs = append(errs, field.Required(spec.Child("selector"), ""))
	case len(sel.MatchLabels)+len(sel.MatchExpressions) == 0:
		errs = append(errs, field.Invalid(spec.Child("selector"), sel, "empty selector is invalid for this kind"))
	default:
		errs = append(errs, metav1validation.ValidateLabelSelector(sel, metav1validation.LabelSelectorValidationOptions{}, spec.Child("selector"))...)
		if s, err := metav1.LabelSelectorAsSelector(sel); err == nil && !s.Matches(labels.Set(tmpl.Labels)) {
			errs = append(errs, field.Invalid(spec.Child("template", "metadata", "labels"), tmpl.Labels, "`selector` does not match template `labels`"))
		}
	}
	errs = append(errs, metav1validation.ValidateLabels(tmpl.Labels, spec.Child("template", "metadata", "labels"))...)
	errs = append(errs, podSpec(&tmpl.Spec, spec.Child("template", "spec"), restart, claims...)...)
	return errs
}

func deployment(d *appsv1.Deployment) field.ErrorList {
	spec := field.NewPath("spec")
	errs := replicas(d.Spec.Replicas, spec.Child("replicas"))
	errs = append(errs, selectorAndTemplate(d.Spec.Selector, &d.Spec.Template, spec, restartAlways)...)

	strategy := spec.Child("strategy")
	switch d.Spec.Strategy.Type {
	case "", appsv1.RollingUpdateDeploymentStrategyType:
		if ru := d.Spec.Strategy.RollingUpdate; ru != nil {
			errs = append(errs, intOrPercent(ru.MaxUnavailable, strategy.Child("rollingUpdate", "maxUnavailable"))...)
			errs = append(errs, intOrPercent(ru.MaxSurge, strategy.Child("rollingUpdate", "maxSurge"))...)
			if isZero(ru.MaxUnavailable) && isZero(ru.MaxSurge) {
				errs = append(errs, field.Invalid(strategy.Child("rollingUpdate", "maxUnavailable"), ru.MaxUnavailable, "may not be 0 when `maxSurge` is 0"))
			}
		}
	case appsv1.RecreateDeploymentStrategyType:
		if d.Spec.Strategy.RollingUpdate != nil {
			errs = append(errs, field.Forbidden(strategy.Child("rollingUpdate"), "may not be specified when strategy `type` is 'Recreate'"))
		}
	default:
		errs = append(errs, field.NotSupported(strategy.Child("type"), string(d.Spec.Strategy.Type), []string{"Recreate", "RollingUpdate"}))
	}
	for name, v := range map[string]*int32{"revisionHistoryLimit": d.Spec.RevisionHistoryLimit, "progressDeadlineSeconds": d.Spec.ProgressDeadlineSeconds} {
		if v != nil && *v < 0 {
			errs = append(errs, field.Invalid(spec.Child(name), *v, "must be greater than or equal to 0"))
		}
	}
	if d.Spec.MinReadySeconds < 0 {
		errs = append(errs, field.Invalid(spec.Child("minReadySeconds"), d.Spec.MinReadySeconds, "must be greater than or equal to 0"))
	}
	return errs
}

func isZero(v *intstr.IntOrString) bool {
	if v == nil {
		return false
	}
	if v.Type == intstr.Int {
		return v.IntVal == 0
	}
	return v.StrVal == "0%"
}

// intOrPercent accepts a non-negative integer or a percentage such as "25%".
func intOrPercent(v *intstr.IntOrString, path *field.Path) field.ErrorList {
	if v == nil {
		return nil
	}
	if v.Type == intstr.Int {
		if v.IntVal < 0 {
			return field.ErrorList{field.Invalid(path, v.IntVal, "must be greater than or equal to 0")}
		}
		return nil
	}
	digits, ok := strings.CutSuffix(v.StrVal, "%")
	if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return field.ErrorList{field.Invalid(path, v.StrVal, "must be an integer or percentage (e.g '5%')")}
	}
	return nil
}

func jobSpec(js *batchv1.JobSpec, spec *field.Path) field.ErrorList {
	var errs field.ErrorList
	for name, v := range map[string]*int32{"parallelism": js.Parallelism, "completions": js.Completions, "backoffLimit": js.BackoffLimit, "ttlSecondsAfterFinished": js.TTLSecondsAfterFinished} {
		if v != nil && *v < 0 {
			errs = append(errs, field.Invalid(spec.Child(name), *v, "must be greater than or equal to 0"))
		}
	}
	if js.ActiveDeadlineSeconds != nil && *js.ActiveDeadlineSeconds < 0 {
		errs = append(errs, field.Invalid(spec.Child("activeDeadlineSeconds"), *js.ActiveDeadlineSeconds, "must be greater than or equal to 0"))
	}
	if js.CompletionMode != nil && *js.CompletionMode == batchv1.IndexedCompletion && js.Completions == nil {
		errs = append(errs, field.Required(spec.Child("completions"), "when completion mode is Indexed"))
	}
	// the API server generates the selector unless manualSelector is set
	if js.ManualSelector != nil && *js.ManualSelector {
		errs = append(errs, selectorAndTemplate(js.Selector, &js.Template, spec, restartNotAlways)...)
		return errs
	}
	errs = append(errs, metav1validation.ValidateLabels(js.Template.Labels, spec.Child("template", "metadata", "labels"))...)
	errs = append(errs, podSpec(&js.Template.Spec, spec.Child("template", "spec"), restartNotAlways)...)
	return errs
}

func cronJob(cj *batchv1.CronJob) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	switch schedule := cj.Spec.Schedule; {
	case schedule == "":
		errs = append(errs, field.Required(spec.Child("schedule"), ""))
	case strings.Contains(schedule, "TZ="):
		errs = append(errs, field.Invalid(spec.Child("schedule"), schedule, "cannot use TZ or CRON_TZ in schedule, use timeZone"))
	default:
		if _, err := cron.ParseStandard(schedule); err != nil {
			errs = append(errs, field.Invalid(spec.Child("schedule"), schedule, err.Error()))
		}
	}
	switch cj.Spec.ConcurrencyPolicy {
	case "", batchv1.AllowConcurrent, batchv1.ForbidConcurrent, batchv1.ReplaceConcurrent:
	default:
		errs = append(errs, field.NotSupported(spec.Child("concurrencyPolicy"), string(cj.Spec.ConcurrencyPolicy), []string{"Allow", "Forbid", "Replace"}))
	}
	if tz := cj.Spec.TimeZone; tz != nil && (*tz == "" || strings.EqualFold(*tz, "Local")) {
		errs = append(errs, field.Invalid(spec.Child("timeZone"), *tz, "must be an explicit IANA time zone name"))
	}
	for name, v := range map[string]*int32{"successfulJobsHistoryLimit": cj.Spec.SuccessfulJobsHistoryLimit, "failedJobsHistoryLimit": cj.Spec.FailedJobsHistoryLimit} {
		if v != nil && *v < 0 {
			errs = append(errs, field.Invalid(spec.Child(name), *v, "must be greater than or equal to 0"))
		}
	}
	if d := cj.Spec.StartingDeadlineSeconds; d != nil && *d < 0 {
		errs = append(errs, field.Invalid(spec.Child("startingDeadlineSeconds"), *d, "must be greater than or equal to 0"))
	}
	errs = append(errs, jobSpec(&cj.Spec.JobTemplate.Spec, spec.Child("jobTemplate", "spec"))...)
	return errs
}
