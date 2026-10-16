package resources

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/source"
)

func fn() *v1alpha1.Function {
	return &v1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "f", Namespace: "ns"},
		Spec: v1alpha1.FunctionSpec{
			Type:          v1alpha1.FunctionTypeService,
			Source:        v1alpha1.Source{Bucket: "bkt", Version: "v1"},
			Architectures: []string{"amd64"},
			Memory:        resource.MustParse("256Mi"),
			Timeout:       metav1.Duration{Duration: 30 * time.Second},
			Concurrency:   4,
		},
	}
}

func resolved() source.Resolved {
	return source.Resolved{Version: "v1", Prefix: "f/v1", ETags: map[string]string{"amd64": "aaa"}}
}

func envMap(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestPodTemplateResources(t *testing.T) {
	pt := PodTemplate(fn(), resolved(), "img:1")
	c := pt.Spec.Containers[0]
	if c.Name != "funcrt" {
		t.Fatal(c.Name)
	}
	lim := c.Resources.Limits[corev1.ResourceMemory]
	if lim.String() != "1088Mi" {
		t.Fatalf("limit %s", lim.String())
	}
	req := c.Resources.Requests[corev1.ResourceMemory]
	if !req.Equal(lim) {
		t.Fatalf("request %s", req.String())
	}
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Fatal("cpu limit set")
	}
	cpu := c.Resources.Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 579 { // ceil(256*4/1769 cores)
		t.Fatalf("cpu %dm", cpu.MilliValue())
	}
	e := envMap(c)
	if e["FUNCRT_CONCURRENCY"].Value != "4" || e["AWS_LAMBDA_FUNCTION_MEMORY_SIZE"].Value != "256" ||
		e["AWS_LAMBDA_FUNCTION_NAME"].Value != "f" || e["AWS_LAMBDA_FUNCTION_VERSION"].Value != "v1" ||
		e["FUNCRT_TIMEOUT"].Value != "30s" || e["FUNCRT_MODE"].Value != "service" {
		t.Fatalf("env %v", e)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.ReadinessProbe.HTTPGet.Port.IntValue() != 9090 {
		t.Fatalf("probe %+v", c.ReadinessProbe)
	}
	if *pt.Spec.TerminationGracePeriodSeconds != 35 {
		t.Fatal(*pt.Spec.TerminationGracePeriodSeconds)
	}
	init := pt.Spec.InitContainers[0]
	if init.Name != "fetcher" || init.Command[0] != "/usr/local/bin/fetcher" {
		t.Fatalf("%+v", init)
	}
	ie := envMap(init)
	if ie["FETCH_PREFIX"].Value != "f/v1" || ie["FETCH_ETAG_AMD64"].Value != "aaa" || ie["FETCH_BUCKET"].Value != "bkt" {
		t.Fatalf("init env %v", ie)
	}
	if ie["S3_ENDPOINT"].ValueFrom.SecretKeyRef.Name != S3SecretName || ie["S3_ENDPOINT"].ValueFrom.SecretKeyRef.Key != "endpoint" {
		t.Fatalf("s3 env %v", ie["S3_ENDPOINT"])
	}
	if pt.Spec.SecurityContext.RunAsUser == nil || *pt.Spec.SecurityContext.RunAsUser != 65532 || *pt.Spec.SecurityContext.FSGroup != 65532 {
		t.Fatal("pod security context")
	}
	if pt.Labels[LabelFunction] != "f" {
		t.Fatal("label")
	}
}

func TestPodTemplateHashChangesWithETag(t *testing.T) {
	a := PodTemplate(fn(), resolved(), "img").Annotations[AnnotationSourceHash]
	r := resolved()
	r.ETags["amd64"] = "bbb"
	b := PodTemplate(fn(), r, "img").Annotations[AnnotationSourceHash]
	if a == "" || a == b {
		t.Fatalf("a=%q b=%q", a, b)
	}
	if a != PodTemplate(fn(), resolved(), "img").Annotations[AnnotationSourceHash] {
		t.Fatal("not deterministic")
	}
}

func TestPodTemplateUserEnvPassThrough(t *testing.T) {
	f := fn()
	f.Spec.Env = []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}}}
	c := PodTemplate(f, resolved(), "img").Spec.Containers[0]
	x := envMap(c)["X"]
	if x.ValueFrom == nil || x.ValueFrom.SecretKeyRef.Name != "s" {
		t.Fatalf("%+v", x)
	}
}

func TestPodTemplatePlatformEnvWinsOverUser(t *testing.T) {
	f := fn()
	f.Spec.Env = []corev1.EnvVar{
		{Name: "FUNCRT_PORT", Value: "1"},
		{Name: "AWS_LAMBDA_FUNCTION_NAME", Value: "evil"},
		{Name: "LOG_LEVEL", Value: "debug"},
	}
	c := PodTemplate(f, resolved(), "img").Spec.Containers[0]
	e := envMap(c) // last duplicate wins, as in Kubernetes
	if e["FUNCRT_PORT"].Value != "8080" || e["AWS_LAMBDA_FUNCTION_NAME"].Value != "f" || e["LOG_LEVEL"].Value != "debug" {
		t.Fatalf("%v", e)
	}
}

func TestPodTemplateArchAffinity(t *testing.T) {
	f := fn()
	f.Spec.Architectures = []string{"amd64", "arm64"}
	pt := PodTemplate(f, source.Resolved{Version: "v1", Prefix: "f/v1", ETags: map[string]string{"amd64": "a", "arm64": "b"}}, "img")
	terms := pt.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	req := terms[0].MatchExpressions[0]
	if req.Key != "kubernetes.io/arch" || req.Operator != corev1.NodeSelectorOpIn || strings.Join(req.Values, ",") != "amd64,arm64" {
		t.Fatalf("%+v", req)
	}
}

func TestHostsIncludesPortForms(t *testing.T) {
	h := Hosts("f", "ns")
	if len(h) != 8 {
		t.Fatal(h)
	}
	found := false
	for _, x := range h {
		found = found || x == "f.ns.svc.cluster.local:8080"
	}
	if !found {
		t.Fatal(h)
	}
}

func TestPublicServiceExternalName(t *testing.T) {
	s := PublicService(fn(), "proxy.keda.svc.cluster.local")
	if s.Spec.Type != corev1.ServiceTypeExternalName || s.Spec.ExternalName != "proxy.keda.svc.cluster.local" || s.Name != "f" {
		t.Fatalf("%+v", s.Spec)
	}
	b := BackendService(fn())
	if b.Name != "f-backend" || b.Spec.Selector[LabelFunction] != "f" || b.Spec.Ports[0].Port != 8080 {
		t.Fatalf("%+v", b)
	}
}

func TestHTTPScaledObject(t *testing.T) {
	o := HTTPScaledObject(fn())
	if o.GetAPIVersion() != "http.keda.sh/v1alpha1" || o.GetKind() != "HTTPScaledObject" || o.GetName() != "f" || o.GetNamespace() != "ns" {
		t.Fatalf("%v", o.Object)
	}
	max, _, _ := unstructuredInt(o.Object, "spec", "replicas", "max")
	if max != 10 {
		t.Fatalf("max %d", max)
	}
	tv, _, _ := unstructuredInt(o.Object, "spec", "scalingMetric", "concurrency", "targetValue")
	if tv != 4 {
		t.Fatalf("target %d", tv)
	}
	if sd, _, _ := unstructuredInt(o.Object, "spec", "scaledownPeriod"); sd != 300 {
		t.Fatalf("default scaledownPeriod %d", sd)
	}
}

func TestHTTPScaledObjectScaledownPeriod(t *testing.T) {
	f := fn()
	p := int32(30)
	f.Spec.Scaling = &v1alpha1.Scaling{Min: 0, Max: 3, ScaledownPeriodSeconds: &p}
	o := HTTPScaledObject(f)
	if sd, _, _ := unstructuredInt(o.Object, "spec", "scaledownPeriod"); sd != 30 {
		t.Fatalf("scaledownPeriod %d", sd)
	}
	// Scaling set without the field (e.g. built without API defaulting) keeps the default.
	f.Spec.Scaling.ScaledownPeriodSeconds = nil
	if sd, _, _ := unstructuredInt(HTTPScaledObject(f).Object, "spec", "scaledownPeriod"); sd != 300 {
		t.Fatalf("fallback scaledownPeriod %d", sd)
	}
}

func TestDeploymentSelector(t *testing.T) {
	d := Deployment(fn(), resolved(), "img")
	if d.Spec.Replicas != nil || d.Spec.Selector.MatchLabels[LabelFunction] != "f" {
		t.Fatalf("%+v", d.Spec)
	}
}

func TestCronJobDeadlineAndPayload(t *testing.T) {
	f := fn()
	f.Spec.Type = v1alpha1.FunctionTypeJob
	f.Spec.Concurrency = 1
	f.Spec.Schedule = "*/5 * * * *"
	f.Spec.Payload = &apiextensionsv1.JSON{Raw: []byte(`{"a":1}`)}
	cj := CronJob(f, resolved(), "img")
	js := cj.Spec.JobTemplate.Spec
	if *js.ActiveDeadlineSeconds != 150 || *js.BackoffLimit != 0 || cj.Spec.ConcurrencyPolicy != "Forbid" || cj.Spec.Schedule != "*/5 * * * *" {
		t.Fatalf("%+v", cj.Spec)
	}
	if js.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("restart")
	}
	e := envMap(js.Template.Spec.Containers[0])
	if e["FUNCRT_PAYLOAD"].Value != `{"a":1}` || e["FUNCRT_MODE"].Value != "job" {
		t.Fatalf("%v", e)
	}
	f.Spec.Payload = nil
	if _, ok := envMap(CronJob(f, resolved(), "img").Spec.JobTemplate.Spec.Template.Spec.Containers[0])["FUNCRT_PAYLOAD"]; ok {
		t.Fatal("payload set")
	}
}
