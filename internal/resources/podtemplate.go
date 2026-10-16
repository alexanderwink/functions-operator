// Package resources builds the Kubernetes objects for a Function. All
// builders are pure functions.
package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/source"
)

const (
	// S3SecretName is the Secret (in the function namespace) with keys
	// endpoint, accessKey, secretKey, useSSL.
	S3SecretName = "functions-s3-read"
	// LabelFunction labels everything belonging to a Function.
	LabelFunction = "functions.pulsen.se/function"
	// AnnotationSourceHash changes whenever the resolved source changes.
	AnnotationSourceHash = "functions.pulsen.se/source-hash"

	runtimeUID   int64 = 65532
	servicePort        = 8080
	adminPort          = 9090
	podOverheadM       = 64 // MiB
	mibPerCPU          = 1769
)

func labels(fn *v1alpha1.Function) map[string]string {
	return map[string]string{LabelFunction: fn.Name}
}

// SourceHash is sha256 over the prefix and the sorted arch=etag pairs.
func SourceHash(r source.Resolved) string {
	pairs := make([]string, 0, len(r.ETags))
	for a, e := range r.ETags {
		pairs = append(pairs, a+"="+e)
	}
	sort.Strings(pairs)
	h := sha256.New()
	h.Write([]byte(r.Prefix))
	for _, p := range pairs {
		h.Write([]byte("\n" + p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func secretEnv(name, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: S3SecretName}, Key: key}}}
}

func containerSecurity() *corev1.SecurityContext {
	f := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func concurrency(fn *v1alpha1.Function) int64 {
	if fn.Spec.Concurrency < 1 {
		return 1
	}
	return int64(fn.Spec.Concurrency)
}

// PodTemplate builds the pod template shared by the Deployment and CronJob.
func PodTemplate(fn *v1alpha1.Function, r source.Resolved, runtimeImage string) corev1.PodTemplateSpec {
	memMiB := fn.Spec.Memory.Value() / (1 << 20)
	conc := concurrency(fn)
	limit := resource.MustParse(strconv.FormatInt(memMiB*conc+podOverheadM, 10) + "Mi")
	cpuM := (memMiB*conc*1000 + mibPerCPU - 1) / mibPerCPU
	if cpuM < 50 {
		cpuM = 50
	}
	mode := "service"
	if fn.Spec.Type == v1alpha1.FunctionTypeJob {
		mode = "job"
	}
	timeout := fn.Spec.Timeout.Duration
	grace := int64(timeout.Seconds()) + 5
	uid := runtimeUID
	nonRoot := true

	fetchEnv := []corev1.EnvVar{
		secretEnv("S3_ENDPOINT", "endpoint"),
		secretEnv("S3_ACCESS_KEY", "accessKey"),
		secretEnv("S3_SECRET_KEY", "secretKey"),
		secretEnv("S3_USE_SSL", "useSSL"),
		{Name: "FETCH_BUCKET", Value: fn.Spec.Source.Bucket},
		{Name: "FETCH_PREFIX", Value: r.Prefix},
	}
	archs := append([]string(nil), fn.Spec.Architectures...)
	for _, a := range archs {
		fetchEnv = append(fetchEnv, corev1.EnvVar{Name: "FETCH_ETAG_" + upper(a), Value: r.ETags[a]})
	}

	// User env comes first so platform values win (Kubernetes uses the last duplicate).
	env := make([]corev1.EnvVar, 0, len(fn.Spec.Env)+10)
	for _, e := range fn.Spec.Env {
		env = append(env, *e.DeepCopy())
	}
	env = append(env, []corev1.EnvVar{
		{Name: "FUNCRT_MODE", Value: mode},
		{Name: "FUNCRT_BOOTSTRAP", Value: "/var/task/bootstrap"},
		{Name: "FUNCRT_CONCURRENCY", Value: strconv.FormatInt(conc, 10)},
		{Name: "FUNCRT_TIMEOUT", Value: timeout.String()},
		{Name: "FUNCRT_PORT", Value: strconv.Itoa(servicePort)},
		{Name: "FUNCRT_ADMIN_PORT", Value: strconv.Itoa(adminPort)},
		{Name: "AWS_LAMBDA_FUNCTION_NAME", Value: fn.Name},
		{Name: "AWS_LAMBDA_FUNCTION_VERSION", Value: r.Version},
		{Name: "AWS_LAMBDA_FUNCTION_MEMORY_SIZE", Value: strconv.FormatInt(memMiB, 10)},
	}...)
	if mode == "job" && fn.Spec.Payload != nil && len(fn.Spec.Payload.Raw) > 0 {
		env = append(env, corev1.EnvVar{Name: "FUNCRT_PAYLOAD", Value: string(fn.Spec.Payload.Raw)})
	}
	task := corev1.VolumeMount{Name: "task", MountPath: "/var/task"}
	tmp := corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels(fn),
			Annotations: map[string]string{AnnotationSourceHash: SourceHash(r)},
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &grace,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &nonRoot,
				RunAsUser:      &uid,
				RunAsGroup:     &uid,
				FSGroup:        &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: archs,
					}}}},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "task", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			InitContainers: []corev1.Container{{
				Name:            "fetcher",
				Image:           runtimeImage,
				Command:         []string{"/usr/local/bin/fetcher"},
				Env:             fetchEnv,
				VolumeMounts:    []corev1.VolumeMount{task},
				SecurityContext: containerSecurity(),
			}},
			Containers: []corev1.Container{{
				Name:  "funcrt",
				Image: runtimeImage,
				Env:   env,
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: servicePort}},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: limit},
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: limit,
						corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuM, resource.DecimalSI),
					},
				},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/readyz", Port: intstr.FromInt32(adminPort)}}},
				VolumeMounts:    []corev1.VolumeMount{task, tmp},
				SecurityContext: containerSecurity(),
			}},
		},
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
