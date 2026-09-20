package validate

import (
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// restartRule is what a pod's owner allows for spec.restartPolicy.
type restartRule int

const (
	anyRestartPolicy restartRule = iota
	restartAlways                // Deployment, StatefulSet, DaemonSet, ReplicaSet
	restartNotAlways             // Job
)

// podSpec validates a pod. claims are volumes that exist without being listed:
// a StatefulSet mounts its volumeClaimTemplates by name.
func podSpec(spec *corev1.PodSpec, path *field.Path, restart restartRule, claims ...string) field.ErrorList {
	var errs field.ErrorList
	if len(spec.Containers) == 0 {
		errs = append(errs, field.Required(path.Child("containers"), ""))
	}

	volumes := map[string]bool{}
	for _, c := range claims {
		volumes[c] = true
	}
	for i, v := range spec.Volumes {
		p := path.Child("volumes").Index(i)
		switch {
		case v.Name == "":
			errs = append(errs, field.Required(p.Child("name"), ""))
		case volumes[v.Name]:
			errs = append(errs, field.Duplicate(p.Child("name"), v.Name))
		default:
			for _, msg := range validation.IsDNS1123Label(v.Name) {
				errs = append(errs, field.Invalid(p.Child("name"), v.Name, msg))
			}
		}
		volumes[v.Name] = true
		switch n := setFields(v.VolumeSource); {
		case n == 0:
			errs = append(errs, field.Required(p, "must specify a volume type"))
		case n > 1:
			errs = append(errs, field.Forbidden(p, "may not specify more than 1 volume type"))
		}
	}

	names := map[string]bool{}
	containers := func(list []corev1.Container, p *field.Path, init bool) {
		for i := range list {
			errs = append(errs, container(&list[i], p.Index(i), names, volumes, init)...)
		}
	}
	containers(spec.InitContainers, path.Child("initContainers"), true)
	containers(spec.Containers, path.Child("containers"), false)

	switch spec.RestartPolicy {
	case "", corev1.RestartPolicyAlways:
		if restart == restartNotAlways {
			errs = append(errs, field.NotSupported(path.Child("restartPolicy"), string(spec.RestartPolicy), []string{"OnFailure", "Never"}))
		}
	case corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever:
		if restart == restartAlways {
			errs = append(errs, field.NotSupported(path.Child("restartPolicy"), string(spec.RestartPolicy), []string{"Always"}))
		}
	default:
		errs = append(errs, field.NotSupported(path.Child("restartPolicy"), string(spec.RestartPolicy), []string{"Always", "OnFailure", "Never"}))
	}
	if g := spec.TerminationGracePeriodSeconds; g != nil && *g < 0 {
		errs = append(errs, field.Invalid(path.Child("terminationGracePeriodSeconds"), *g, "must be greater than or equal to 0"))
	}
	if spec.ServiceAccountName != "" {
		for _, msg := range validation.IsDNS1123Subdomain(spec.ServiceAccountName) {
			errs = append(errs, field.Invalid(path.Child("serviceAccountName"), spec.ServiceAccountName, msg))
		}
	}
	for i, s := range spec.ImagePullSecrets {
		if s.Name == "" {
			errs = append(errs, field.Required(path.Child("imagePullSecrets").Index(i).Child("name"), ""))
		}
	}
	return errs
}

func container(c *corev1.Container, path *field.Path, names, volumes map[string]bool, init bool) field.ErrorList {
	var errs field.ErrorList
	switch {
	case c.Name == "":
		errs = append(errs, field.Required(path.Child("name"), ""))
	case names[c.Name]:
		errs = append(errs, field.Duplicate(path.Child("name"), c.Name))
	default:
		for _, msg := range validation.IsDNS1123Label(c.Name) {
			errs = append(errs, field.Invalid(path.Child("name"), c.Name, msg))
		}
	}
	names[c.Name] = true
	if c.Image == "" {
		errs = append(errs, field.Required(path.Child("image"), ""))
	}
	switch c.ImagePullPolicy {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		errs = append(errs, field.NotSupported(path.Child("imagePullPolicy"), string(c.ImagePullPolicy), []string{"Always", "IfNotPresent", "Never"}))
	}

	portNames := map[string]bool{}
	for i, p := range c.Ports {
		pp := path.Child("ports").Index(i)
		if p.Name != "" {
			for _, msg := range validation.IsValidPortName(p.Name) {
				errs = append(errs, field.Invalid(pp.Child("name"), p.Name, msg))
			}
			if portNames[p.Name] {
				errs = append(errs, field.Duplicate(pp.Child("name"), p.Name))
			}
			portNames[p.Name] = true
		}
		if p.ContainerPort == 0 {
			errs = append(errs, field.Required(pp.Child("containerPort"), ""))
		} else {
			errs = append(errs, portNumber(int(p.ContainerPort), pp.Child("containerPort"))...)
		}
		if p.HostPort != 0 {
			errs = append(errs, portNumber(int(p.HostPort), pp.Child("hostPort"))...)
		}
		errs = append(errs, protocol(p.Protocol, pp.Child("protocol"))...)
	}

	for i, e := range c.Env {
		ep := path.Child("env").Index(i)
		if e.Name == "" {
			errs = append(errs, field.Required(ep.Child("name"), ""))
		}
		if e.ValueFrom == nil {
			continue
		}
		switch n := setFields(*e.ValueFrom); {
		case e.Value != "" && n > 0:
			errs = append(errs, field.Invalid(ep.Child("valueFrom"), "", "may not be specified when `value` is not empty"))
		case n == 0:
			errs = append(errs, field.Invalid(ep.Child("valueFrom"), "", "must specify one of: `fieldRef`, `resourceFieldRef`, `configMapKeyRef` or `secretKeyRef`"))
		case n > 1:
			errs = append(errs, field.Invalid(ep.Child("valueFrom"), "", "may not have more than one field specified at a time"))
		}
		if r := e.ValueFrom.SecretKeyRef; r != nil && r.Key == "" {
			errs = append(errs, field.Required(ep.Child("valueFrom", "secretKeyRef", "key"), ""))
		}
		if r := e.ValueFrom.ConfigMapKeyRef; r != nil && r.Key == "" {
			errs = append(errs, field.Required(ep.Child("valueFrom", "configMapKeyRef", "key"), ""))
		}
	}

	mountPaths := map[string]bool{}
	for i, m := range c.VolumeMounts {
		mp := path.Child("volumeMounts").Index(i)
		switch {
		case m.Name == "":
			errs = append(errs, field.Required(mp.Child("name"), ""))
		case !volumes[m.Name]:
			errs = append(errs, field.NotFound(mp.Child("name"), m.Name))
		}
		switch {
		case m.MountPath == "":
			errs = append(errs, field.Required(mp.Child("mountPath"), ""))
		case mountPaths[m.MountPath]:
			errs = append(errs, field.Invalid(mp.Child("mountPath"), m.MountPath, "must be unique"))
		}
		mountPaths[m.MountPath] = true
	}

	for name, limit := range c.Resources.Limits {
		if request, ok := c.Resources.Requests[name]; ok && request.Cmp(limit) > 0 {
			errs = append(errs, field.Invalid(path.Child("resources", "requests"), request.String(),
				fmt.Sprintf("must be less than or equal to %s limit of %s", name, limit.String())))
		}
	}
	for name, q := range c.Resources.Requests {
		if q.Sign() < 0 {
			errs = append(errs, field.Invalid(path.Child("resources", "requests").Key(string(name)), q.String(), "must be greater than or equal to 0"))
		}
	}

	probes := map[string]*corev1.Probe{"livenessProbe": c.LivenessProbe, "readinessProbe": c.ReadinessProbe, "startupProbe": c.StartupProbe}
	for name, p := range probes {
		if p == nil {
			continue
		}
		pp := path.Child(name)
		// a sidecar (an init container that restarts) may have probes
		if init && (c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways) {
			errs = append(errs, field.Forbidden(pp, "may not be set for init containers without restartPolicy=Always"))
			continue
		}
		switch n := setFields(p.ProbeHandler); {
		case n == 0:
			errs = append(errs, field.Required(pp, "must specify a handler type"))
		case n > 1:
			errs = append(errs, field.Forbidden(pp, "may not specify more than 1 handler type"))
		}
		for fname, v := range map[string]int32{"initialDelaySeconds": p.InitialDelaySeconds, "timeoutSeconds": p.TimeoutSeconds,
			"periodSeconds": p.PeriodSeconds, "successThreshold": p.SuccessThreshold, "failureThreshold": p.FailureThreshold} {
			if v < 0 {
				errs = append(errs, field.Invalid(pp.Child(fname), v, "must be greater than or equal to 0"))
			}
		}
		if name != "readinessProbe" && p.SuccessThreshold > 1 {
			errs = append(errs, field.Invalid(pp.Child("successThreshold"), p.SuccessThreshold, "must be 1"))
		}
	}
	return errs
}

func portNumber(n int, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	for _, msg := range validation.IsValidPortNum(n) {
		errs = append(errs, field.Invalid(path, n, msg))
	}
	return errs
}

func protocol(p corev1.Protocol, path *field.Path) field.ErrorList {
	switch p {
	case "", corev1.ProtocolTCP, corev1.ProtocolUDP, corev1.ProtocolSCTP:
		return nil
	}
	return field.ErrorList{field.NotSupported(path, string(p), []string{"TCP", "UDP", "SCTP"})}
}

// setFields counts the pointer fields of a union struct that are set. The API
// expresses "exactly one of" as a struct of optional pointers: a volume source,
// a probe handler, an env var source.
func setFields(union any) int {
	v := reflect.ValueOf(union)
	n := 0
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Pointer && !f.IsNil() {
			n++
		}
	}
	return n
}
