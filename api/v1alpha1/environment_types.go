package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types reported on Environment.status.conditions.
const (
	ConditionReady       = "Ready"       // all workloads are available
	ConditionProgressing = "Progressing" // reconcile is still converging
	ConditionDegraded    = "Degraded"    // something is wrong and needs attention
)

// WorkloadSpec describes one Deployment + Service pair.
type WorkloadSpec struct {
	// Name is used for both the Deployment and the Service, so it must be a DNS label.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Image is the container image to run.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Port is the container port, also exposed by the Service.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Replicas defaults to 1. 0 is allowed (scaled-down preview).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// EnvironmentSpec is the desired state.
type EnvironmentSpec struct {
	// TTL is how long the environment lives, measured from metadata.creationTimestamp.
	// Go duration syntax, seconds/minutes/hours only (e.g. "30m", "2h", "1h30m").
	// Because it is anchored to creation time, editing it extends or shortens the lifetime.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	TTL string `json:"ttl"`

	// Workloads are keyed by name; the map list-type makes server-side apply merge per item.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +listType=map
	// +listMapKey=name
	Workloads []WorkloadSpec `json:"workloads"`

	// Labels are propagated to the namespace and every object created in it.
	// Keys with the "preview.example.dev/" prefix are reserved for the controller.
	// +kubebuilder:validation:MaxProperties=20
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('preview.example.dev/'))",message="label keys must not use the reserved preview.example.dev/ prefix"
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// EnvironmentStatus is the observed state.
type EnvironmentStatus struct {
	// ObservedGeneration is the spec generation the conditions were computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ExpiresAt is creationTimestamp + spec.ttl, recorded for humans and dashboards.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Namespace is the name of the namespace this environment owns.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Conditions: Ready, Progressing, Degraded. Managed with meta.SetStatusCondition.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Environment is cluster-scoped so it can own the (cluster-scoped) Namespace
// through a real ownerReference. Name is capped so "mayfly-<name>" fits in 63 chars.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=env
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 50",message="metadata.name must be at most 50 characters"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Environment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EnvironmentSpec   `json:"spec"`
	Status EnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EnvironmentList contains a list of Environment.
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Environment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Environment{}, &EnvironmentList{})
		return nil
	})
}
