package resources

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/source"
)

// jobHeadroomSeconds covers fetch and init time on top of the invocation timeout.
const jobHeadroomSeconds = 120

// CronJob builds the scheduled workload for a job-type Function.
func CronJob(fn *v1alpha1.Function, r source.Resolved, runtimeImage string) *batchv1.CronJob {
	pt := PodTemplate(fn, r, runtimeImage)
	pt.Spec.RestartPolicy = corev1.RestartPolicyNever
	deadline := int64(fn.Spec.Timeout.Seconds()) + jobHeadroomSeconds
	backoff := int32(0)
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace, Labels: labels(fn)},
		Spec: batchv1.CronJobSpec{
			Schedule:          fn.Spec.Schedule,
			ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(fn)},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: &deadline,
					BackoffLimit:          &backoff,
					Template:              pt,
				},
			},
		},
	}
}
