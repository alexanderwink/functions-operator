package controller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/alexanderwink/functions-operator/internal/resources"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "functions-operator"
)

// copyS3Secret mirrors the operator's read-only S3 Secret into ns so the
// fetcher init container can reference it. Secrets are read with the
// uncached APIReader so the manager never caches Secrets cluster-wide. The
// copy has no ownerRef: it is shared by every Function in the namespace.
func (r *FunctionReconciler) copyS3Secret(ctx context.Context, ns string) error {
	if ns == r.S3SecretNamespace {
		return nil
	}
	src := &corev1.Secret{}
	key := client.ObjectKey{Namespace: r.S3SecretNamespace, Name: resources.S3SecretName}
	if err := r.reader().Get(ctx, key, src); err != nil {
		return fmt.Errorf("reading S3 secret %s: %w", key, err)
	}

	dst := &corev1.Secret{}
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: resources.S3SecretName}, dst)
	switch {
	case apierrors.IsNotFound(err):
		dst = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: resources.S3SecretName, Namespace: ns,
				Labels: map[string]string{labelManagedBy: managedByValue},
			},
			Type: src.Type,
			Data: maps.Clone(src.Data),
		}
		if err := r.Create(ctx, dst); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating S3 secret in %s: %w", ns, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading S3 secret in %s: %w", ns, err)
	}

	if dst.Labels[labelManagedBy] == managedByValue && equality.Semantic.DeepEqual(dst.Data, src.Data) {
		return nil
	}
	if dst.Labels == nil {
		dst.Labels = map[string]string{}
	}
	dst.Labels[labelManagedBy] = managedByValue
	dst.Data = maps.Clone(src.Data)
	if err := r.Update(ctx, dst); err != nil {
		return fmt.Errorf("updating S3 secret in %s: %w", ns, err)
	}
	return nil
}

func (r *FunctionReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
