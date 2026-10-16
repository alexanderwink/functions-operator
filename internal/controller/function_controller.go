/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	functionsv1alpha1 "github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/resources"
	"github.com/alexanderwink/functions-operator/internal/source"
)

// HTTPScaledObjectGVK is the KEDA HTTP add-on kind the operator manages.
var HTTPScaledObjectGVK = schema.GroupVersionKind{Group: "http.keda.sh", Version: "v1alpha1", Kind: "HTTPScaledObject"}

const (
	defaultArch = "amd64"

	reasonResolved      = "Resolved"
	reasonObjectMissing = "ObjectMissing"
	reasonS3Error       = "S3Error"
	reasonDeployed      = "Deployed"
	reasonSourceError   = "SourceNotResolved"
	reasonS3Secret      = "S3SecretError"
	reasonInvalidSpec   = "InvalidSpec"
	reasonApplyError    = "ReconcileError"
)

// FunctionReconciler reconciles a Function object
type FunctionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads Secrets without the cache. Defaults to Client.
	APIReader          client.Reader
	Resolver           source.Resolver
	RuntimeImage       string
	InterceptorHost    string
	S3SecretNamespace  string
	LatestPollInterval time.Duration
	Now                func() time.Time
}

// +kubebuilder:rbac:groups=functions.pulsen.se,resources=functions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=functions.pulsen.se,resources=functions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=functions.pulsen.se,resources=functions/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// Secrets: create cannot be restricted by name (the name is not known to
// authorization before the object exists); everything else only on the S3
// Secret. This also covers the operator's startup read of the source Secret.
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=create
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;update;patch,resourceNames=functions-s3-read
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=http.keda.sh,resources=httpscaledobjects,verbs=get;list;watch;create;update;patch;delete

// Reconcile resolves the Function's source in S3 and converges its workloads.
func (r *FunctionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	fn := &functionsv1alpha1.Function{}
	if err := r.Get(ctx, req.NamespacedName, fn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := fn.DeepCopy()

	var result ctrl.Result
	if fn.Spec.Source.Version == "latest" {
		result.RequeueAfter = r.LatestPollInterval
	}

	// 1. Credentials for the fetcher.
	if err := r.copyS3Secret(ctx, fn.Namespace); err != nil {
		r.setCondition(fn, functionsv1alpha1.ConditionReady, metav1.ConditionFalse, reasonS3Secret, err.Error())
		return result, errors.Join(err, r.patchStatus(ctx, fn, orig))
	}

	// 2. Resolve. On failure leave workloads exactly as they are.
	resolved, err := r.Resolver.Resolve(ctx, fn.Spec.Source.Bucket, sourceKey(fn), fn.Spec.Source.Version, architectures(fn))
	if err != nil {
		reason := reasonS3Error
		if errors.Is(err, source.ErrObjectMissing) {
			reason = reasonObjectMissing
		}
		log.Info("source not resolved", "reason", reason, "error", err.Error())
		r.setCondition(fn, functionsv1alpha1.ConditionSourceResolved, metav1.ConditionFalse, reason, err.Error())
		r.setCondition(fn, functionsv1alpha1.ConditionReady, metav1.ConditionFalse, reasonSourceError,
			"source could not be resolved; existing workloads left unchanged")
		fn.Status.ObservedGeneration = fn.Generation
		return ctrl.Result{RequeueAfter: r.LatestPollInterval}, r.patchStatus(ctx, fn, orig)
	}
	r.setCondition(fn, functionsv1alpha1.ConditionSourceResolved, metav1.ConditionTrue, reasonResolved,
		fmt.Sprintf("s3://%s/%s", fn.Spec.Source.Bucket, resolved.Prefix))

	// 3. Workloads.
	var readyMsg string
	switch fn.Spec.Type {
	case functionsv1alpha1.FunctionTypeJob:
		err = r.applyJob(ctx, fn, resolved)
		readyMsg = "CronJob applied"
	default:
		err = r.applyService(ctx, fn, resolved)
		readyMsg = "Deployment and HTTPScaledObject applied"
	}
	fn.Status.ObservedGeneration = fn.Generation
	if err != nil {
		if apierrors.IsInvalid(err) {
			// The spec cannot be turned into valid objects (e.g. a bad cron
			// schedule). Retrying will not help until the spec changes.
			log.Info("generated objects rejected", "error", err.Error())
			r.setCondition(fn, functionsv1alpha1.ConditionReady, metav1.ConditionFalse, reasonInvalidSpec, err.Error())
			return result, r.patchStatus(ctx, fn, orig)
		}
		r.setCondition(fn, functionsv1alpha1.ConditionReady, metav1.ConditionFalse, reasonApplyError, err.Error())
		return result, errors.Join(err, r.patchStatus(ctx, fn, orig))
	}

	// 4. Status.
	fn.Status.ResolvedVersion = resolved.Version
	fn.Status.ETags = resolved.ETags
	r.setCondition(fn, functionsv1alpha1.ConditionReady, metav1.ConditionTrue, reasonDeployed, readyMsg)
	return result, r.patchStatus(ctx, fn, orig)
}

func (r *FunctionReconciler) applyService(ctx context.Context, fn *functionsv1alpha1.Function, res source.Resolved) error {
	wantDep := resources.Deployment(fn, res, r.RuntimeImage)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: wantDep.Name, Namespace: wantDep.Namespace}}
	if err := r.apply(ctx, fn, dep, func() {
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Selector = wantDep.Spec.Selector // immutable after create
		}
		// Spec.Replicas is deliberately untouched: KEDA owns it.
		dep.Spec.Template = wantDep.Spec.Template
		mergeLabels(dep, wantDep.Labels)
	}); err != nil {
		return err
	}

	for _, want := range []*corev1.Service{resources.BackendService(fn), resources.PublicService(fn, r.InterceptorHost)} {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
		if err := r.apply(ctx, fn, svc, func() {
			// Only the fields we own; ClusterIP(s) etc. stay as allocated.
			svc.Spec.Type = want.Spec.Type
			svc.Spec.ExternalName = want.Spec.ExternalName
			svc.Spec.Selector = want.Spec.Selector
			svc.Spec.Ports = want.Spec.Ports
			mergeLabels(svc, want.Labels)
		}); err != nil {
			return err
		}
	}

	wantHSO := resources.HTTPScaledObject(fn)
	hso := newHSO(fn.Namespace, fn.Name)
	if err := r.apply(ctx, fn, hso, func() {
		hso.Object["spec"] = wantHSO.Object["spec"]
		mergeLabels(hso, wantHSO.GetLabels())
	}); err != nil {
		return err
	}

	return r.deleteOwned(ctx, fn, &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace}})
}

func (r *FunctionReconciler) applyJob(ctx context.Context, fn *functionsv1alpha1.Function, res source.Resolved) error {
	want := resources.CronJob(fn, res, r.RuntimeImage)
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	if err := r.apply(ctx, fn, cj, func() {
		cj.Spec.Schedule = want.Spec.Schedule
		cj.Spec.ConcurrencyPolicy = want.Spec.ConcurrencyPolicy
		cj.Spec.JobTemplate = want.Spec.JobTemplate
		mergeLabels(cj, want.Labels)
	}); err != nil {
		return err
	}

	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fn.Name + "-backend", Namespace: fn.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace}},
		newHSO(fn.Namespace, fn.Name),
	} {
		if err := r.deleteOwned(ctx, fn, obj); err != nil {
			return err
		}
	}
	return nil
}

// apply create-or-updates obj, running mutate on the live (or new) object and
// making fn its controller.
func (r *FunctionReconciler) apply(ctx context.Context, fn *functionsv1alpha1.Function, obj client.Object, mutate func()) error {
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		mutate()
		return controllerutil.SetControllerReference(fn, obj, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("%s %s: %w", kindOf(obj), obj.GetName(), err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("applied", "kind", kindOf(obj), "name", obj.GetName(), "operation", op)
	}
	return nil
}

// deleteOwned deletes obj if it exists and is controlled by fn. Objects with
// the same name that belong to someone else are left alone.
func (r *FunctionReconciler) deleteOwned(ctx context.Context, fn *functionsv1alpha1.Function, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("%s %s: %w", kindOf(obj), obj.GetName(), err)
	}
	if !metav1.IsControlledBy(obj, fn) || obj.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting %s %s: %w", kindOf(obj), obj.GetName(), err)
	}
	logf.FromContext(ctx).Info("deleted", "kind", kindOf(obj), "name", obj.GetName())
	return nil
}

func (r *FunctionReconciler) setCondition(fn *functionsv1alpha1.Function, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&fn.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: fn.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
}

func (r *FunctionReconciler) patchStatus(ctx context.Context, fn, orig *functionsv1alpha1.Function) error {
	if err := r.Status().Patch(ctx, fn, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("patching status: %w", err)
	}
	return nil
}

func (r *FunctionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func sourceKey(fn *functionsv1alpha1.Function) string {
	if fn.Spec.Source.Key != "" {
		return fn.Spec.Source.Key
	}
	return fn.Name
}

func architectures(fn *functionsv1alpha1.Function) []string {
	if len(fn.Spec.Architectures) == 0 {
		return []string{defaultArch}
	}
	return fn.Spec.Architectures
}

func mergeLabels(obj metav1.Object, want map[string]string) {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	for k, v := range want {
		l[k] = v
	}
	obj.SetLabels(l)
}

func newHSO(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(HTTPScaledObjectGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func kindOf(obj client.Object) string {
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		return o.GetKind()
	case *appsv1.Deployment:
		return "Deployment"
	case *corev1.Service:
		return "Service"
	case *batchv1.CronJob:
		return "CronJob"
	default:
		return fmt.Sprintf("%T", obj)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *FunctionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	hso := &unstructured.Unstructured{}
	hso.SetGroupVersionKind(HTTPScaledObjectGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&functionsv1alpha1.Function{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.CronJob{}).
		Owns(hso).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podToFunction),
			builder.WithPredicates(etagMismatchPredicate())).
		Named("function").
		Complete(r)
}

const (
	fetcherContainerName = "fetcher"
	fetcherExitETag      = 3
	etagMismatchMessage  = "etag-mismatch"
)

// hasETagMismatch reports whether the pod's fetcher init container terminated
// because the S3 object no longer matches the resolved ETag. After a kubelet
// restart the termination sits in LastTerminationState.
func hasETagMismatch(pod *corev1.Pod) bool {
	for _, st := range pod.Status.InitContainerStatuses {
		if st.Name != fetcherContainerName {
			continue
		}
		for _, t := range []*corev1.ContainerStateTerminated{st.State.Terminated, st.LastTerminationState.Terminated} {
			if t != nil && t.ExitCode == fetcherExitETag && strings.TrimSpace(t.Message) == etagMismatchMessage {
				return true
			}
		}
	}
	return false
}

func isETagMismatchPod(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	if _, ok := pod.Labels[resources.LabelFunction]; !ok {
		return false
	}
	return hasETagMismatch(pod)
}

// etagMismatchPredicate passes Create and Update events of pods whose fetcher
// reported an ETag mismatch; status changes arrive as updates.
func etagMismatchPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isETagMismatchPod(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isETagMismatchPod(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func podToFunction(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[resources.LabelFunction]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: name}}}
}
