package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/resources"
	"github.com/alexanderwink/functions-operator/internal/source"
)

const operatorNS = "functions-operator-system"

var (
	k8sClient client.Client
	testCfg   *rest.Config
	counter   atomic.Int64
)

func TestMain(m *testing.M) {
	code := 1
	defer func() { os.Exit(code) }()

	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "config", "test-crds"),
		},
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
	testCfg = cfg
	k8sClient, err = client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		return
	}
	ctx := context.Background()
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNS}}); err != nil {
		fmt.Fprintln(os.Stderr, "namespace:", err)
		return
	}
	if err := k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: resources.S3SecretName, Namespace: operatorNS},
		StringData: map[string]string{
			"endpoint": "minio:9000", "accessKey": "ak", "secretKey": "sk", "useSSL": "false",
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "secret:", err)
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

// fakeResolver serves ETags per version; a version without entries is missing.
type fakeResolver struct {
	mu    sync.Mutex
	etags map[string]map[string]string // version -> arch -> etag
	err   error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{etags: map[string]map[string]string{}}
}

func (f *fakeResolver) set(version, arch, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.etags[version] == nil {
		f.etags[version] = map[string]string{}
	}
	f.etags[version][arch] = etag
}

func (f *fakeResolver) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeResolver) Resolve(_ context.Context, bucket, key, version string, archs []string) (source.Resolved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return source.Resolved{}, f.err
	}
	res := source.Resolved{Version: version, Prefix: key + "/" + version, ETags: map[string]string{}}
	for _, a := range archs {
		e, ok := f.etags[version][a]
		if !ok {
			return source.Resolved{}, fmt.Errorf("%w: %s/%s/%s/linux-%s/bootstrap", source.ErrObjectMissing, bucket, key, version, a)
		}
		res.ETags[a] = e
	}
	return res, nil
}

type fixture struct {
	t   *testing.T
	ctx context.Context
	ns  string
	fr  *fakeResolver
	r   *FunctionReconciler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	ns := fmt.Sprintf("team-%d", counter.Add(1))
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	fr := newFakeResolver()
	return &fixture{t: t, ctx: ctx, ns: ns, fr: fr, r: &FunctionReconciler{
		Client:             k8sClient,
		APIReader:          k8sClient,
		Scheme:             k8sClient.Scheme(),
		Resolver:           fr,
		RuntimeImage:       "runtime:test",
		InterceptorHost:    "keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local",
		S3SecretNamespace:  operatorNS,
		LatestPollInterval: 30 * time.Second,
		Now:                func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) },
	}}
}

func (f *fixture) createFn(name string, typ v1alpha1.FunctionType, version string) *v1alpha1.Function {
	f.t.Helper()
	fn := &v1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: v1alpha1.FunctionSpec{
			Type:   typ,
			Source: v1alpha1.Source{Bucket: "bkt", Version: version},
		},
	}
	if typ == v1alpha1.FunctionTypeJob {
		fn.Spec.Schedule = "*/5 * * * *"
	}
	if err := k8sClient.Create(f.ctx, fn); err != nil {
		f.t.Fatal(err)
	}
	return fn
}

func (f *fixture) reconcile(name string) ctrl.Result {
	f.t.Helper()
	res, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name}})
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (f *fixture) get(name string, obj client.Object) {
	f.t.Helper()
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: name}, obj); err != nil {
		f.t.Fatalf("get %T %s: %v", obj, name, err)
	}
}

func (f *fixture) gone(name string, obj client.Object) {
	f.t.Helper()
	err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: name}, obj)
	if err == nil && obj.GetDeletionTimestamp() == nil {
		f.t.Fatalf("%T %s still exists", obj, name)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		f.t.Fatalf("get %T %s: %v", obj, name, err)
	}
}

func (f *fixture) fn(name string) *v1alpha1.Function {
	fn := &v1alpha1.Function{}
	f.get(name, fn)
	return fn
}

func (f *fixture) update(name string, mutate func(*v1alpha1.Function)) {
	f.t.Helper()
	fn := f.fn(name)
	mutate(fn)
	if err := k8sClient.Update(f.ctx, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) deploymentHash(name string) string {
	d := &appsv1.Deployment{}
	f.get(name, d)
	return d.Spec.Template.Annotations[resources.AnnotationSourceHash]
}

func hso() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("http.keda.sh/v1alpha1")
	u.SetKind("HTTPScaledObject")
	return u
}

func condStatus(t *testing.T, fn *v1alpha1.Function, typ string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(fn.Status.Conditions, typ)
	if c == nil {
		t.Fatalf("condition %s missing: %+v", typ, fn.Status.Conditions)
	}
	return c
}

func assertOwned(t *testing.T, fn *v1alpha1.Function, obj client.Object) {
	t.Helper()
	if !metav1.IsControlledBy(obj, fn) {
		t.Fatalf("%T %s not controlled by function: %+v", obj, obj.GetName(), obj.GetOwnerReferences())
	}
}

func TestReconcileServiceCreatesObjects(t *testing.T) {
	f := newFixture(t)
	f.fr.set("v1", "amd64", "e1")
	f.createFn("hello", v1alpha1.FunctionTypeService, "v1")

	res := f.reconcile("hello")
	if res.RequeueAfter != 0 {
		t.Fatalf("pinned version should not requeue, got %v", res.RequeueAfter)
	}
	fn := f.fn("hello")

	d := &appsv1.Deployment{}
	f.get("hello", d)
	assertOwned(t, fn, d)
	if d.Spec.Template.Annotations[resources.AnnotationSourceHash] == "" {
		t.Fatal("deployment template has no source hash")
	}
	if d.Spec.Template.Spec.Containers[0].Image != "runtime:test" {
		t.Fatalf("image %q", d.Spec.Template.Spec.Containers[0].Image)
	}

	backend := &corev1.Service{}
	f.get("hello-backend", backend)
	assertOwned(t, fn, backend)
	if backend.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("backend type %q", backend.Spec.Type)
	}

	pub := &corev1.Service{}
	f.get("hello", pub)
	assertOwned(t, fn, pub)
	if pub.Spec.Type != corev1.ServiceTypeExternalName ||
		pub.Spec.ExternalName != "keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local" {
		t.Fatalf("public service %+v", pub.Spec)
	}

	h := hso()
	f.get("hello", h)
	assertOwned(t, fn, h)
	svc, _, _ := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "service")
	if svc != "hello-backend" {
		t.Fatalf("hso service %q", svc)
	}

	sec := &corev1.Secret{}
	f.get(resources.S3SecretName, sec)
	if string(sec.Data["endpoint"]) != "minio:9000" || string(sec.Data["secretKey"]) != "sk" {
		t.Fatalf("secret copy data %v", sec.Data)
	}
	if sec.Labels["app.kubernetes.io/managed-by"] != "functions-operator" {
		t.Fatalf("secret labels %v", sec.Labels)
	}
	if len(sec.OwnerReferences) != 0 {
		t.Fatalf("secret copy must not have ownerRefs: %v", sec.OwnerReferences)
	}

	if c := condStatus(t, fn, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready %+v", c)
	}
	if c := condStatus(t, fn, v1alpha1.ConditionSourceResolved); c.Status != metav1.ConditionTrue {
		t.Fatalf("SourceResolved %+v", c)
	}
	if fn.Status.ResolvedVersion != "v1" || fn.Status.ETags["amd64"] != "e1" {
		t.Fatalf("status %+v", fn.Status)
	}
	if fn.Status.ObservedGeneration != fn.Generation {
		t.Fatalf("observedGeneration %d, generation %d", fn.Status.ObservedGeneration, fn.Generation)
	}

	// A second reconcile is a no-op that keeps everything Ready.
	f.reconcile("hello")
	d2 := &appsv1.Deployment{}
	f.get("hello", d2)
	if d2.Generation != d.Generation {
		t.Fatalf("deployment generation changed on no-op reconcile: %d -> %d", d.Generation, d2.Generation)
	}
	h2 := hso()
	f.get("hello", h2)
	if h2.GetGeneration() != h.GetGeneration() {
		t.Fatalf("hso generation changed on no-op reconcile")
	}
	if c := condStatus(t, f.fn("hello"), v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready after second reconcile %+v", c)
	}
}

func TestReconcileJobCreatesCronJob(t *testing.T) {
	f := newFixture(t)
	f.fr.set("v1", "amd64", "e1")
	f.createFn("nightly", v1alpha1.FunctionTypeJob, "v1")

	f.reconcile("nightly")
	fn := f.fn("nightly")

	cj := &batchv1.CronJob{}
	f.get("nightly", cj)
	assertOwned(t, fn, cj)
	if cj.Spec.Schedule != "*/5 * * * *" {
		t.Fatalf("schedule %q", cj.Spec.Schedule)
	}
	if cj.Spec.JobTemplate.Spec.Template.Annotations[resources.AnnotationSourceHash] == "" {
		t.Fatal("cronjob template has no source hash")
	}
	f.gone("nightly", &appsv1.Deployment{})
	f.gone("nightly-backend", &corev1.Service{})
	f.gone("nightly", hso())
	f.get(resources.S3SecretName, &corev1.Secret{})

	if c := condStatus(t, fn, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready %+v", c)
	}
}

func TestReconcileInvalidScheduleReportsNotReady(t *testing.T) {
	f := newFixture(t)
	f.fr.set("v1", "amd64", "e1")
	fn := &v1alpha1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "badcron", Namespace: f.ns},
		Spec: v1alpha1.FunctionSpec{
			Type: v1alpha1.FunctionTypeJob, Schedule: "not a cron",
			Source: v1alpha1.Source{Bucket: "bkt", Version: "v1"},
		},
	}
	if err := k8sClient.Create(f.ctx, fn); err != nil {
		t.Fatal(err)
	}
	f.reconcile("badcron") // must not return an error (no crash loop)
	c := condStatus(t, f.fn("badcron"), v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "InvalidSpec" {
		t.Fatalf("Ready %+v", c)
	}
	f.gone("badcron", &batchv1.CronJob{})
}

func TestReconcileVersionChangeUpdatesHash(t *testing.T) {
	f := newFixture(t)
	f.fr.set("v1", "amd64", "e1")
	f.fr.set("v2", "amd64", "e2")
	f.createFn("hello", v1alpha1.FunctionTypeService, "v1")
	f.reconcile("hello")
	h1 := f.deploymentHash("hello")

	f.update("hello", func(fn *v1alpha1.Function) { fn.Spec.Source.Version = "v2" })
	f.reconcile("hello")
	h2 := f.deploymentHash("hello")
	if h1 == h2 {
		t.Fatal("source hash did not change on version change")
	}
	fn := f.fn("hello")
	if fn.Status.ResolvedVersion != "v2" || fn.Status.ETags["amd64"] != "e2" {
		t.Fatalf("status %+v", fn.Status)
	}
	if fn.Status.ObservedGeneration != fn.Generation {
		t.Fatalf("observedGeneration %d, generation %d", fn.Status.ObservedGeneration, fn.Generation)
	}
}

func TestReconcileLatestRequeuesAndRollsOnETagChange(t *testing.T) {
	f := newFixture(t)
	f.fr.set("latest", "amd64", "e1")
	f.createFn("dev", v1alpha1.FunctionTypeService, "latest")
	res := f.reconcile("dev")
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter %v", res.RequeueAfter)
	}
	h1 := f.deploymentHash("dev")

	f.fr.set("latest", "amd64", "e2")
	res = f.reconcile("dev")
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter %v", res.RequeueAfter)
	}
	if f.deploymentHash("dev") == h1 {
		t.Fatal("source hash did not change on ETag change")
	}
	if got := f.fn("dev").Status.ETags["amd64"]; got != "e2" {
		t.Fatalf("status etag %q", got)
	}
}

func TestReconcileSourceMissingKeepsDeployment(t *testing.T) {
	f := newFixture(t)
	f.fr.set("v1", "amd64", "e1")
	f.createFn("hello", v1alpha1.FunctionTypeService, "v1")
	f.reconcile("hello")
	h1 := f.deploymentHash("hello")

	// Point at a version that does not exist.
	f.update("hello", func(fn *v1alpha1.Function) { fn.Spec.Source.Version = "v9" })
	res := f.reconcile("hello")
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter %v", res.RequeueAfter)
	}
	fn := f.fn("hello")
	sr := condStatus(t, fn, v1alpha1.ConditionSourceResolved)
	if sr.Status != metav1.ConditionFalse || sr.Reason != "ObjectMissing" {
		t.Fatalf("SourceResolved %+v", sr)
	}
	if c := condStatus(t, fn, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready %+v", c)
	}
	if f.deploymentHash("hello") != h1 {
		t.Fatal("deployment changed although source could not be resolved")
	}
	if fn.Status.ResolvedVersion != "v1" {
		t.Fatalf("resolvedVersion should still report the deployed version, got %q", fn.Status.ResolvedVersion)
	}

	// A generic S3 failure is reported as S3Error.
	f.fr.setErr(fmt.Errorf("connection refused"))
	f.reconcile("hello")
	if sr := condStatus(t, f.fn("hello"), v1alpha1.ConditionSourceResolved); sr.Reason != "S3Error" {
		t.Fatalf("SourceResolved %+v", sr)
	}
	if f.deploymentHash("hello") != h1 {
		t.Fatal("deployment changed on S3 error")
	}
}

func TestReconcileTypeSwitchCleansUp(t *testing.T) {
	f := newFixture(t)
	f.fr.set("v1", "amd64", "e1")
	f.createFn("sw", v1alpha1.FunctionTypeService, "v1")
	f.reconcile("sw")
	f.get("sw", &appsv1.Deployment{})

	f.update("sw", func(fn *v1alpha1.Function) {
		fn.Spec.Type = v1alpha1.FunctionTypeJob
		fn.Spec.Schedule = "0 * * * *"
		fn.Spec.Scaling = nil
	})
	f.reconcile("sw")
	f.gone("sw", &appsv1.Deployment{})
	f.gone("sw-backend", &corev1.Service{})
	f.gone("sw", &corev1.Service{})
	f.gone("sw", hso())
	cj := &batchv1.CronJob{}
	f.get("sw", cj)
	assertOwned(t, f.fn("sw"), cj)

	// And back again.
	f.update("sw", func(fn *v1alpha1.Function) {
		fn.Spec.Type = v1alpha1.FunctionTypeService
		fn.Spec.Schedule = ""
	})
	f.reconcile("sw")
	f.gone("sw", &batchv1.CronJob{})
	f.get("sw", &appsv1.Deployment{})
	f.get("sw", hso())
}

func TestReconcileDoesNotOverwriteReplicas(t *testing.T) {
	f := newFixture(t)
	f.fr.set("v1", "amd64", "e1")
	f.fr.set("v2", "amd64", "e2")
	f.createFn("hello", v1alpha1.FunctionTypeService, "v1")
	f.reconcile("hello")

	d := &appsv1.Deployment{}
	f.get("hello", d)
	three := int32(3)
	d.Spec.Replicas = &three
	if err := k8sClient.Update(f.ctx, d); err != nil {
		t.Fatal(err)
	}

	f.reconcile("hello")
	f.update("hello", func(fn *v1alpha1.Function) { fn.Spec.Source.Version = "v2" })
	f.reconcile("hello")

	d = &appsv1.Deployment{}
	f.get("hello", d)
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 3 {
		t.Fatalf("replicas %v", d.Spec.Replicas)
	}
}

func TestReconcileDeletedFunctionIsNoop(t *testing.T) {
	f := newFixture(t)
	f.reconcile("does-not-exist")
}
