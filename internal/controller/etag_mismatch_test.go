package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/resources"
	"github.com/alexanderwink/functions-operator/internal/source"
)

func TestPodETagMismatchTriggersReconcile(t *testing.T) {
	f := newFixture(t)
	f.r.LatestPollInterval = time.Hour

	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:  k8sClient.Scheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.r.Client = mgr.GetClient()
	if err := f.r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	f.fr.set("latest", "amd64", "e1")
	f.createFn("dev", v1alpha1.FunctionTypeService, "latest")
	h1 := waitHash(t, f, "dev", func(h string) bool { return h != "" })

	f.fr.set("latest", "amd64", "e2")
	want := resources.SourceHash(source.Resolved{Version: "latest", Prefix: "dev/latest", ETags: map[string]string{"amd64": "e2"}})
	if want == h1 {
		t.Fatal("test setup: hashes equal")
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-abc", Namespace: f.ns, Labels: map[string]string{resources.LabelFunction: "dev"}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "fetcher", Image: "fetcher:test"}},
			Containers:     []corev1.Container{{Name: "app", Image: "app:test"}},
		},
	}
	if err := k8sClient.Create(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "fetcher",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 3, Message: "etag-mismatch\n",
		}},
	}}
	if err := k8sClient.Status().Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	waitHash(t, f, "dev", func(h string) bool { return h == want })
}

func waitHash(t *testing.T, f *fixture, name string, ok func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := ""
		if d, err := getDeployment(f, name); err == nil {
			h = d
		}
		if ok(h) {
			return h
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for hash; last %q", h)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func getDeployment(f *fixture, name string) (string, error) {
	d := &appsv1.Deployment{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: name}, d); err != nil {
		return "", err
	}
	return d.Spec.Template.Annotations[resources.AnnotationSourceHash], nil
}
