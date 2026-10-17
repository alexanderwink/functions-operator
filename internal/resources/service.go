package resources

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/source"
)

const defaultScaledownSeconds int64 = 300

// Deployment builds the service Deployment. Replicas is left nil: KEDA owns it.
func Deployment(fn *v1alpha1.Function, r source.Resolved, runtimeImage string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace, Labels: labels(fn)},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels(fn)},
			Template: PodTemplate(fn, r, runtimeImage),
		},
	}
}

// BackendService selects the function pods; the HTTPScaledObject targets it.
func BackendService(fn *v1alpha1.Function) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: fn.Name + "-backend", Namespace: fn.Namespace, Labels: labels(fn)},
		Spec: corev1.ServiceSpec{
			Selector: labels(fn),
			Ports: []corev1.ServicePort{{
				Name: portName, Port: servicePort, TargetPort: intstr.FromInt32(servicePort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// PublicService is the in-cluster name callers use; it points at the KEDA interceptor.
func PublicService(fn *v1alpha1.Function, interceptorHost string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace, Labels: labels(fn)},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: interceptorHost,
			Ports:        []corev1.ServicePort{{Name: portName, Port: servicePort, Protocol: corev1.ProtocolTCP}},
		},
	}
}

// Hosts lists the host forms the interceptor should route to the function.
// The bare name is deliberately absent (ruling R18): it would be ambiguous
// across namespaces in the shared interceptor routing table. Callers use
// <name>.<ns> or longer.
func Hosts(name, ns string) []string {
	bases := []string{name + "." + ns, name + "." + ns + ".svc", name + "." + ns + ".svc.cluster.local"}
	out := make([]string, 0, 2*len(bases))
	for _, b := range bases {
		out = append(out, b, b+":8080")
	}
	return out
}

// HTTPScaledObject builds the KEDA HTTP add-on object (http.keda.sh/v1alpha1).
func HTTPScaledObject(fn *v1alpha1.Function) *unstructured.Unstructured {
	min, max, scaledown := int64(0), int64(10), defaultScaledownSeconds
	if s := fn.Spec.Scaling; s != nil {
		min, max = int64(s.Min), int64(s.Max)
		if s.ScaledownPeriodSeconds != nil {
			scaledown = int64(*s.ScaledownPeriodSeconds)
		}
	}
	hosts := Hosts(fn.Name, fn.Namespace)
	hs := make([]any, len(hosts))
	for i, h := range hosts {
		hs[i] = h
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "http.keda.sh/v1alpha1",
		"kind":       "HTTPScaledObject",
		"metadata": map[string]any{
			"name": fn.Name, "namespace": fn.Namespace,
			"labels": map[string]any{LabelFunction: fn.Name},
		},
		"spec": map[string]any{
			"hosts": hs,
			"scaleTargetRef": map[string]any{
				"name": fn.Name, "kind": "Deployment", "apiVersion": "apps/v1",
				"service": fn.Name + "-backend", "port": int64(servicePort),
			},
			"replicas":        map[string]any{"min": min, "max": max},
			"scalingMetric":   map[string]any{"concurrency": map[string]any{"targetValue": concurrency(fn)}},
			"scaledownPeriod": scaledown,
		},
	}}
}
