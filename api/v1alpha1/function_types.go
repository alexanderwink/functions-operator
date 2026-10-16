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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// FunctionType selects how a Function is run.
// +kubebuilder:validation:Enum=service;job
type FunctionType string

const (
	// FunctionTypeService runs the function as an autoscaled HTTP service.
	FunctionTypeService FunctionType = "service"
	// FunctionTypeJob runs the function on a cron schedule.
	FunctionTypeJob FunctionType = "job"
)

const (
	// ConditionReady reports whether the function is ready to serve or run.
	ConditionReady = "Ready"
	// ConditionSourceResolved reports whether the S3 source has been resolved.
	ConditionSourceResolved = "SourceResolved"
)

// Source locates the function bootstrap binaries in S3.
type Source struct {
	// bucket is the S3 bucket holding the function.
	// +kubebuilder:validation:MinLength=1
	// +required
	Bucket string `json:"bucket"`

	// key is the S3 key prefix. Defaults to metadata.name when empty.
	// +optional
	Key string `json:"key,omitempty"`

	// version is a fixed version string, or "latest".
	// +kubebuilder:validation:MinLength=1
	// +required
	Version string `json:"version"`
}

// Scaling configures replica bounds for services.
type Scaling struct {
	// min is the minimum number of replicas (0 allows scale to zero).
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	Min int32 `json:"min"`

	// max is the maximum number of replicas.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +optional
	Max int32 `json:"max"`
}

// FunctionSpec defines the desired state of Function
// +kubebuilder:validation:XValidation:rule="self.type == 'job' ? has(self.schedule) : !has(self.schedule)",message="schedule is required for jobs and forbidden for services"
// +kubebuilder:validation:XValidation:rule="self.type == 'service' || !has(self.scaling)",message="scaling is only allowed for services"
// +kubebuilder:validation:XValidation:rule="self.type == 'service' || !has(self.concurrency) || self.concurrency == 1",message="concurrency must be 1 for jobs"
// +kubebuilder:validation:XValidation:rule="!has(self.scaling) || self.scaling.min <= self.scaling.max",message="scaling.min must be <= scaling.max"
type FunctionSpec struct {
	// type is "service" (HTTP) or "job" (cron).
	// +required
	Type FunctionType `json:"type"`

	// source locates the bootstrap binaries in S3.
	// +required
	Source Source `json:"source"`

	// architectures to run on.
	// +kubebuilder:default={"amd64"}
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=amd64;arm64
	// +optional
	Architectures []string `json:"architectures,omitempty"`

	// env is the environment passed to the function.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// memory per bootstrap copy.
	// +kubebuilder:default="128Mi"
	// +optional
	Memory resource.Quantity `json:"memory,omitzero"`

	// timeout per invocation.
	// +kubebuilder:default="30s"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitzero"`

	// concurrency is the number of bootstrap copies per pod.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`

	// scaling bounds replicas (services only).
	// +optional
	Scaling *Scaling `json:"scaling,omitempty"`

	// schedule is a cron expression (jobs only).
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// payload is the event passed to a job instead of the default scheduled event.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Payload *apiextensionsv1.JSON `json:"payload,omitempty"`
}

// FunctionStatus defines the observed state of Function.
type FunctionStatus struct {
	// resolvedVersion is the concrete version currently deployed.
	// +optional
	ResolvedVersion string `json:"resolvedVersion,omitempty"`

	// etags maps architecture to the S3 ETag of the resolved bootstrap.
	// +optional
	ETags map[string]string `json:"etags,omitempty"`

	// observedGeneration is the last reconciled metadata.generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the Function resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fn
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.source.version`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Function is the Schema for the functions API
type Function struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Function
	// +required
	Spec FunctionSpec `json:"spec"`

	// status defines the observed state of Function
	// +optional
	Status FunctionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FunctionList contains a list of Function
type FunctionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Function `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Function{}, &FunctionList{})
		return nil
	})
}
