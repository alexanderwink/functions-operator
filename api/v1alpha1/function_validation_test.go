package v1alpha1

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func createRaw(t *testing.T, spec map[string]any) error {
	t.Helper()
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GroupVersion.String(),
		"kind":       "Function",
		"metadata": map[string]any{
			"name":      fmt.Sprintf("raw-%d", counter.Add(1)),
			"namespace": "default",
		},
		"spec": spec,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return k8sClient.Create(ctx, u)
}

func rawSpec(over map[string]any) map[string]any {
	spec := map[string]any{
		"type":   "service",
		"source": map[string]any{"bucket": "b", "version": "latest"},
	}
	maps.Copy(spec, over)
	return spec
}

func TestRejectsNonPositiveTimeout(t *testing.T) {
	for _, v := range []string{"0s", "-5s"} {
		if err := createRaw(t, rawSpec(map[string]any{"timeout": v})); err == nil {
			t.Errorf("timeout %q should be rejected", v)
		}
	}
	if err := createRaw(t, rawSpec(map[string]any{"timeout": "5s"})); err != nil {
		t.Errorf("timeout 5s should be accepted: %v", err)
	}
}

func TestRejectsNonPositiveMemory(t *testing.T) {
	for _, v := range []any{"0", int64(0), "-1Mi"} {
		if err := createRaw(t, rawSpec(map[string]any{"memory": v})); err == nil {
			t.Errorf("memory %v should be rejected", v)
		}
	}
	if err := createRaw(t, rawSpec(map[string]any{"memory": "256Mi"})); err != nil {
		t.Errorf("memory 256Mi should be accepted: %v", err)
	}
}

func TestSourceSegments(t *testing.T) {
	bad := []string{"../x", "a/b", "..", "/v1", ".hidden"}
	good := []string{"v42", "latest", "orders-api", "1.2.3"}
	for _, field := range []string{"key", "version"} {
		for _, v := range bad {
			src := map[string]any{"bucket": "b", "version": "latest"}
			src[field] = v
			if err := createRaw(t, rawSpec(map[string]any{"source": src})); err == nil {
				t.Errorf("%s %q should be rejected", field, v)
			}
		}
		for _, v := range good {
			src := map[string]any{"bucket": "b", "version": "latest"}
			src[field] = v
			if err := createRaw(t, rawSpec(map[string]any{"source": src})); err != nil {
				t.Errorf("%s %q should be accepted: %v", field, v, err)
			}
		}
	}
}

func TestEnvReservedNames(t *testing.T) {
	env := func(n string) map[string]any {
		return rawSpec(map[string]any{"env": []any{map[string]any{"name": n, "value": "x"}}})
	}
	for _, n := range []string{"FUNCRT_PORT", "AWS_LAMBDA_FUNCTION_NAME", "_HANDLER", "LAMBDA_TASK_ROOT"} {
		if err := createRaw(t, env(n)); err == nil {
			t.Errorf("env %q should be rejected", n)
		}
	}
	for _, n := range []string{"LOG_LEVEL", "AWS_REGION"} {
		if err := createRaw(t, env(n)); err != nil {
			t.Errorf("env %q should be accepted: %v", n, err)
		}
	}
}

func TestScaledownPeriodDefaultAndMinimum(t *testing.T) {
	f := newFn(FunctionTypeService)
	f.Spec.Scaling = &Scaling{Min: 0, Max: 2}
	if err := create(t, f); err != nil {
		t.Fatal(err)
	}
	got := &Function{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	if p := got.Spec.Scaling.ScaledownPeriodSeconds; p == nil || *p != 300 {
		t.Errorf("scaledownPeriodSeconds = %v, want 300", p)
	}

	zero := int32(0)
	f = newFn(FunctionTypeService)
	f.Spec.Scaling = &Scaling{Min: 0, Max: 2, ScaledownPeriodSeconds: &zero}
	if err := create(t, f); err != nil {
		t.Errorf("scaledownPeriodSeconds 0 should be accepted: %v", err)
	}

	if err := createRaw(t, rawSpec(map[string]any{
		"scaling": map[string]any{"max": int64(2), "scaledownPeriodSeconds": int64(-1)},
	})); err == nil {
		t.Error("expected negative scaledownPeriodSeconds to be rejected")
	}
}

func namedFn(name string, t FunctionType) *Function {
	f := newFn(t)
	f.Name = name
	if t == FunctionTypeJob {
		f.Spec.Schedule = "*/5 * * * *"
	}
	return f
}

func TestNameLengthLimits(t *testing.T) {
	name := func(prefix string, n int) string {
		b := []byte(prefix)
		for len(b) < n {
			b = append(b, 'a')
		}
		return string(b)
	}
	for _, tc := range []struct {
		name string
		typ  FunctionType
		ok   bool
	}{
		{name("s55-", 55), FunctionTypeService, true},
		{name("s56-", 56), FunctionTypeService, false},
		{name("j52-", 52), FunctionTypeJob, true},
		{name("j53-", 53), FunctionTypeJob, false},
	} {
		err := create(t, namedFn(tc.name, tc.typ))
		if tc.ok && err != nil {
			t.Errorf("%s %q (%d chars): rejected: %v", tc.typ, tc.name, len(tc.name), err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s %q (%d chars): accepted; want rejected", tc.typ, tc.name, len(tc.name))
		}
	}
}

func TestNameMustStartWithLetter(t *testing.T) {
	if err := create(t, namedFn("1abc", FunctionTypeService)); err == nil {
		t.Error("name starting with a digit accepted; want rejected (DNS-1035)")
	}
	if err := create(t, namedFn("abc-1", FunctionTypeService)); err != nil {
		t.Errorf("abc-1 rejected: %v", err)
	}
}
