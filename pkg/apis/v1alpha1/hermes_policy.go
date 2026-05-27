package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// HermesPolicyList
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type HermesPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HermesPolicy `json:"items"`
}

// HermesPolicy controls which observed Pod images Hermes should build SOCI
// indexes for.
// +genclient
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:shortName=hp,scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Paused",type="boolean",JSONPath=".spec.paused"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Failed",type="integer",JSONPath=".status.failed"
type HermesPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              HermesPolicySpec   `json:"spec"`
	Status            HermesPolicyStatus `json:"status,omitempty"`
}

type HermesPolicySpec struct {
	// Paused disables new automatic build enqueueing for this policy.
	// +optional
	Paused bool `json:"paused,omitempty"`
	// ImageSelectors matches Pod image references. Selectors are ORed together.
	// Only imageRegex is supported intentionally; use regular expressions such
	// as ".*vllm.*".
	// +optional
	ImageSelectors []HermesImageSelector `json:"imageSelectors,omitempty"`
	// Platforms lists platforms to build. When empty, the controller default
	// platform is used.
	// +optional
	Platforms []string `json:"platforms,omitempty"`
}

type HermesImageSelector struct {
	// ImageRegex is matched against the raw image reference from Pod specs.
	ImageRegex string `json:"imageRegex"`
}

type HermesPolicyStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	Ready              int32               `json:"ready,omitempty"`
	Failed             int32               `json:"failed,omitempty"`
	Images             []HermesImageStatus `json:"images,omitempty"`
}

type HermesImageStatus struct {
	ImageDigestRef string           `json:"imageDigestRef"`
	Platform       string           `json:"platform"`
	Phase          HermesImagePhase `json:"phase"`
	LastBuildTime  metav1.Time      `json:"lastBuildTime,omitempty"`
	Error          string           `json:"error,omitempty"`
}

// +kubebuilder:validation:Enum=Building;Ready;Failed
type HermesImagePhase string

const (
	HermesImagePhaseBuilding HermesImagePhase = "Building"
	HermesImagePhaseReady    HermesImagePhase = "Ready"
	HermesImagePhaseFailed   HermesImagePhase = "Failed"
)
