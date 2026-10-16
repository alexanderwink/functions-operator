package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	k8sClient client.Client
	counter   atomic.Int64
)

func TestMain(m *testing.M) {
	code := 1
	defer func() { os.Exit(code) }()

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := AddToScheme(s); err != nil {
		panic(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" && os.Getenv("KUBEBUILDER_ASSETS") == "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest start:", err)
		return
	}
	defer func() { _ = env.Stop() }()
	k8sClient, err = client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		return
	}
	code = m.Run()
}

func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

func newFn(t FunctionType) *Function {
	return &Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("fn-%d", counter.Add(1)),
			Namespace: "default",
		},
		Spec: FunctionSpec{
			Type:   t,
			Source: Source{Bucket: "b", Version: "latest"},
		},
	}
}

func create(t *testing.T, f *Function) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return k8sClient.Create(ctx, f)
}

func TestRejectsJobWithoutSchedule(t *testing.T) {
	if err := create(t, newFn(FunctionTypeJob)); err == nil {
		t.Fatal("expected job without schedule to be rejected")
	}
}

func TestRejectsServiceWithSchedule(t *testing.T) {
	f := newFn(FunctionTypeService)
	f.Spec.Schedule = "*/5 * * * *"
	if err := create(t, f); err == nil {
		t.Fatal("expected service with schedule to be rejected")
	}
}

func TestRejectsJobWithScaling(t *testing.T) {
	f := newFn(FunctionTypeJob)
	f.Spec.Schedule = "*/5 * * * *"
	f.Spec.Scaling = &Scaling{Min: 0, Max: 2}
	if err := create(t, f); err == nil {
		t.Fatal("expected job with scaling to be rejected")
	}
}

func TestAcceptsJobWithoutConcurrency(t *testing.T) {
	f := newFn(FunctionTypeJob)
	f.Spec.Schedule = "*/5 * * * *"
	if err := create(t, f); err != nil {
		t.Fatalf("job with schedule should be accepted: %v", err)
	}
}

func TestRejectsScalingMinAboveMax(t *testing.T) {
	f := newFn(FunctionTypeService)
	f.Spec.Scaling = &Scaling{Min: 5, Max: 2}
	if err := create(t, f); err == nil {
		t.Fatal("expected min > max to be rejected")
	}
}

func TestDefaults(t *testing.T) {
	f := newFn(FunctionTypeService)
	if err := create(t, f); err != nil {
		t.Fatal(err)
	}
	got := &Function{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Architectures) != 1 || got.Spec.Architectures[0] != "amd64" {
		t.Errorf("architectures = %v", got.Spec.Architectures)
	}
	if got.Spec.Concurrency != 1 {
		t.Errorf("concurrency = %d", got.Spec.Concurrency)
	}
	if got.Spec.Timeout.Duration != 30*time.Second {
		t.Errorf("timeout = %v", got.Spec.Timeout.Duration)
	}
	if got.Spec.Memory.String() != "128Mi" {
		t.Errorf("memory = %s", got.Spec.Memory.String())
	}
}
