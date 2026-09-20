package validate

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

var decoder = json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme, json.SerializerOptions{Strict: true})

func check(t *testing.T, manifest string) []string {
	t.Helper()
	b, err := yaml.YAMLToJSON([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	obj, _, err := decoder.Decode(b, nil, nil)
	if err != nil {
		t.Fatalf("the manifest must decode, this package is about what comes after: %v", err)
	}
	var out []string
	for _, e := range Object(obj) {
		out = append(out, e.Error())
	}
	return out
}

const validDeployment = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop, labels: {app.kubernetes.io/name: web}}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxSurge: 25%, maxUnavailable: 0}}
  template:
    metadata: {labels: {app: web, tier: front}}
    spec:
      serviceAccountName: web
      imagePullSecrets: [{name: regcred}]
      initContainers:
        - name: migrate
          image: example.test/web:1
        - name: proxy
          image: example.test/proxy:1
          restartPolicy: Always
          readinessProbe: {tcpSocket: {port: 15000}}
      containers:
        - name: web
          image: example.test/web:1
          ports: [{name: http, containerPort: 8080}, {name: metrics, containerPort: 9090, protocol: TCP}]
          env:
            - {name: MODE, value: production}
            - name: TOKEN
              valueFrom: {secretKeyRef: {name: web, key: token}}
          resources: {requests: {cpu: 100m, memory: 64Mi}, limits: {memory: 128Mi}}
          livenessProbe: {httpGet: {path: /healthz, port: http}}
          volumeMounts: [{name: cache, mountPath: /cache}, {name: config, mountPath: /etc/web}]
      volumes:
        - {name: cache, emptyDir: {}}
        - name: config
          configMap: {name: web}
`

// Nothing here is unusual: if one of these is reported, the package is wrong.
func TestValidObjectsPass(t *testing.T) {
	for name, manifest := range map[string]string{
		"deployment": validDeployment,
		"service": `
apiVersion: v1
kind: Service
metadata: {name: web, namespace: shop}
spec:
  selector: {app: web}
  ports: [{name: http, port: 80, targetPort: http}, {name: metrics, port: 9090, targetPort: 9090}]
`,
		"headless service without ports": `
apiVersion: v1
kind: Service
metadata: {name: db}
spec: {clusterIP: None, selector: {app: db}}
`,
		"external name": `
apiVersion: v1
kind: Service
metadata: {name: upstream}
spec: {type: ExternalName, externalName: api.example.test}
`,
		"job with generateName": `
apiVersion: batch/v1
kind: Job
metadata: {generateName: migrate-}
spec:
  backoffLimit: 3
  template:
    spec:
      restartPolicy: OnFailure
      containers: [{name: migrate, image: example.test/migrate:1}]
`,
		"cronjob": `
apiVersion: batch/v1
kind: CronJob
metadata: {name: report}
spec:
  schedule: "*/15 * * * *"
  timeZone: Europe/Dublin
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers: [{name: report, image: example.test/report:1}]
`,
		"cluster role binding with colons": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: "system:web:reader"}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects:
  - {kind: ServiceAccount, name: web, namespace: shop}
  - {kind: Group, name: "system:authenticated", apiGroup: rbac.authorization.k8s.io}
`,
		"role binding without subject namespace": `
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: web, namespace: shop}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: web}
subjects: [{kind: ServiceAccount, name: web}]
`,
		"ingress": `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web}
spec:
  rules:
    - host: "*.example.test"
      http:
        paths:
          - path: /
            pathType: Prefix
            backend: {service: {name: web, port: {name: http}}}
`,
		"statefulset": `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec:
  serviceName: db
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers:
        - name: db
          image: example.test/db:1
          volumeMounts: [{name: data, mountPath: /data}]
  volumeClaimTemplates:
    - metadata: {name: data}
      spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 10Gi}}}
`,
		"tls secret": `
apiVersion: v1
kind: Secret
metadata: {name: web-tls}
type: kubernetes.io/tls
stringData: {tls.crt: cert, tls.key: key}
`,
		"hpa and pdb": `
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: web}
spec: {minReplicas: 2, maxReplicas: 10, scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: web}}
`,
		"pdb": `
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: web}
spec: {maxUnavailable: 25%, selector: {matchLabels: {app: web}}}
`,
		"a kind this package knows nothing about": `
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: "fast.ssd"}
provisioner: example.test/ssd
`,
	} {
		if errs := check(t, manifest); len(errs) != 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// Each case changes one thing about a valid object and names the error the
// API server gives for it.
func TestRejectedValues(t *testing.T) {
	edit := func(old, new string) string {
		if !strings.Contains(validDeployment, old) {
			t.Fatalf("the deployment has no %q", old)
		}
		return strings.Replace(validDeployment, old, new, 1)
	}
	for name, tc := range map[string]struct{ manifest, want string }{
		"selector does not match the template": {edit("selector: {matchLabels: {app: web}}", "selector: {matchLabels: {app: api}}"),
			"spec.template.metadata.labels: Invalid value"},
		"empty selector":       {edit("selector: {matchLabels: {app: web}}", "selector: {}"), "spec.selector: Invalid value"},
		"port out of range":    {edit("containerPort: 8080", "containerPort: 80800"), "spec.template.spec.containers[0].ports[0].containerPort: Invalid value: 80800"},
		"port name too long":   {edit("name: metrics, containerPort", "name: metrics-endpoint-http, containerPort"), "must be no more than 15 characters"},
		"duplicate port name":  {edit("name: metrics, containerPort", "name: http, containerPort"), `ports[1].name: Duplicate value: "http"`},
		"unknown protocol":     {edit("protocol: TCP", "protocol: HTTP"), `Unsupported value: "HTTP"`},
		"mount without volume": {edit("{name: cache, emptyDir: {}}", "{name: scratch, emptyDir: {}}"), `volumeMounts[0].name: Not found: "cache"`},
		"volume without type":  {edit("{name: cache, emptyDir: {}}", "{name: cache}"), "must specify a volume type"},
		"two volume types":     {edit("{name: cache, emptyDir: {}}", "{name: cache, emptyDir: {}, secret: {secretName: x}}"), "may not specify more than 1 volume type"},
		"value and valueFrom":  {edit("- name: TOKEN\n", "- name: TOKEN\n              value: literal\n"), "may not be specified when `value` is not empty"},
		"request above limit":  {edit("memory: 64Mi", "memory: 256Mi"), "must be less than or equal to memory limit of 128Mi"},
		"restart policy":       {edit("serviceAccountName: web\n", "serviceAccountName: web\n      restartPolicy: OnFailure\n"), `restartPolicy: Unsupported value: "OnFailure"`},
		"probe on a plain init container": {edit("image: example.test/web:1\n        - name: proxy", "image: example.test/web:1\n          livenessProbe: {exec: {command: [ok]}}\n        - name: proxy"),
			"may not be set for init containers without restartPolicy=Always"},
		"probe without handler":   {edit("livenessProbe: {httpGet: {path: /healthz, port: http}}", "livenessProbe: {periodSeconds: 5}"), "must specify a handler type"},
		"container name":          {edit("- name: web\n          image", "- name: Web_App\n          image"), "spec.template.spec.containers[0].name: Invalid value"},
		"duplicate container":     {edit("- name: proxy", "- name: migrate"), `Duplicate value: "migrate"`},
		"no image":                {edit("- name: web\n          image: example.test/web:1", "- name: web"), "containers[0].image: Required value"},
		"pull policy":             {edit("- name: web\n", "- name: web\n          imagePullPolicy: Sometimes\n"), `Unsupported value: "Sometimes"`},
		"surge and unavailable 0": {edit("maxSurge: 25%", "maxSurge: 0"), "may not be 0 when `maxSurge` is 0"},
		"recreate with rolling":   {edit("type: RollingUpdate", "type: Recreate"), "may not be specified when strategy `type` is 'Recreate'"},
		"negative replicas":       {edit("replicas: 2", "replicas: -1"), "spec.replicas: Invalid value: -1"},
		"object name":             {edit("metadata: {name: web,", "metadata: {name: Web_Frontend,"), "metadata.name: Invalid value"},
		"namespace name":          {edit("namespace: shop", "namespace: Shop.Front"), "metadata.namespace: Invalid value"},
		"label value":             {edit("app.kubernetes.io/name: web", `app.kubernetes.io/name: "web frontend"`), "metadata.labels: Invalid value"},

		"service port": {`
apiVersion: v1
kind: Service
metadata: {name: web}
spec: {ports: [{port: 70000}]}
`, "spec.ports[0].port: Invalid value: 70000"},
		"service ports need names": {`
apiVersion: v1
kind: Service
metadata: {name: web}
spec: {ports: [{port: 80}, {port: 443}]}
`, "spec.ports[0].name: Required value"},
		"service name starts with a digit": {`
apiVersion: v1
kind: Service
metadata: {name: 1web}
spec: {ports: [{port: 80}]}
`, "metadata.name: Invalid value"},
		"service without ports": {`
apiVersion: v1
kind: Service
metadata: {name: web}
spec: {selector: {app: web}}
`, "spec.ports: Required value"},
		"node port on a ClusterIP service": {`
apiVersion: v1
kind: Service
metadata: {name: web}
spec: {ports: [{port: 80, nodePort: 30080}]}
`, "may not be used when `type` is 'ClusterIP'"},
		"job that restarts always": {`
apiVersion: batch/v1
kind: Job
metadata: {name: migrate}
spec:
  template:
    spec:
      containers: [{name: migrate, image: example.test/migrate:1}]
`, "restartPolicy: Unsupported value"},
		"cron schedule": {`
apiVersion: batch/v1
kind: CronJob
metadata: {name: report}
spec:
  schedule: "*/15 * * *"
  jobTemplate: {spec: {template: {spec: {restartPolicy: Never, containers: [{name: r, image: i}]}}}}
`, "spec.schedule: Invalid value"},
		"cron time zone in the schedule": {`
apiVersion: batch/v1
kind: CronJob
metadata: {name: report}
spec:
  schedule: "CRON_TZ=UTC 0 5 * * *"
  jobTemplate: {spec: {template: {spec: {restartPolicy: Never, containers: [{name: r, image: i}]}}}}
`, "use timeZone"},
		"ingress path type": {`
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web}
spec:
  rules:
    - http:
        paths:
          - path: /
            backend: {service: {name: web, port: {number: 80}}}
`, "pathType: Required value"},
		"ingress backend port": {`
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web}
spec:
  defaultBackend: {service: {name: web, port: {name: http, number: 80}}}
`, "cannot set both port name & port number"},
		"claim without access mode": {`
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec: {resources: {requests: {storage: 1Gi}}}
`, "spec.accessModes: Required value"},
		"config map key": {`
apiVersion: v1
kind: ConfigMap
metadata: {name: web}
data: {"app/config.yaml": x}
`, `data[app/config.yaml]: Invalid value`},
		"tls secret without key": {`
apiVersion: v1
kind: Secret
metadata: {name: web-tls}
type: kubernetes.io/tls
stringData: {tls.crt: cert}
`, "data[tls.key]: Required value"},
		"hpa bounds": {`
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: web}
spec: {minReplicas: 5, maxReplicas: 3, scaleTargetRef: {kind: Deployment, name: web}}
`, "must be greater than or equal to `minReplicas`"},
		"pdb with both": {`
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: web}
spec: {minAvailable: 1, maxUnavailable: 1}
`, "cannot be both set"},
		"role binding to an unknown kind": {`
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: web}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Roles, name: web}
subjects: [{kind: ServiceAccount, name: web}]
`, `roleRef.kind: Unsupported value: "Roles"`},
		"cluster role binding subject without namespace": {`
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: web}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects: [{kind: ServiceAccount, name: web}]
`, "subjects[0].namespace: Required value"},
	} {
		errs := check(t, tc.manifest)
		found := false
		for _, e := range errs {
			found = found || strings.Contains(e, tc.want)
		}
		if !found {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, errs)
		}
		if len(errs) > 2 {
			t.Errorf("%s: one mistake should not produce %d errors: %v", name, len(errs), errs)
		}
	}
}
