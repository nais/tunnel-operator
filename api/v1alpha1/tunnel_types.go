package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type TunnelPhase string

const (
	TunnelPhasePending      TunnelPhase = "Pending"
	TunnelPhaseProvisioning TunnelPhase = "Provisioning"
	TunnelPhaseReady        TunnelPhase = "Ready"
	TunnelPhaseConnected    TunnelPhase = "Connected"
	TunnelPhaseFailed       TunnelPhase = "Failed"
	TunnelPhaseTerminated   TunnelPhase = "Terminated"
)

type TunnelTargetPodSelector struct {
	MatchLabels      map[string]string                 `json:"matchLabels,omitempty"`
	MatchExpressions []metav1.LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.resolvedIP) && size(self.resolvedIP) > 0) != has(self.podSelector)",message="exactly one of resolvedIP or podSelector must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.podSelector) || (has(self.podSelector.matchLabels) && size(self.podSelector.matchLabels) > 0) || (has(self.podSelector.matchExpressions) && size(self.podSelector.matchExpressions) > 0)",message="podSelector must not be empty"
type TunnelTarget struct {
	Host string `json:"host"`
	Port int32  `json:"port"`

	// ResolvedIP is the external target address. It is mutually exclusive with PodSelector.
	// +optional
	ResolvedIP string `json:"resolvedIP,omitempty"`

	// PodSelector identifies permitted in-cluster target Pods in the Tunnel namespace.
	// It is mutually exclusive with ResolvedIP.
	// +optional
	PodSelector *TunnelTargetPodSelector `json:"podSelector,omitempty"`
}

type TunnelSpec struct {
	TeamSlug              string       `json:"teamSlug"`
	Environment           string       `json:"environment"`
	Target                TunnelTarget `json:"target"`
	ClientPublicKey       string       `json:"clientPublicKey"`
	ActiveDeadlineSeconds *int64       `json:"activeDeadlineSeconds,omitempty"`
}

type TunnelStatus struct {
	Phase             TunnelPhase        `json:"phase,omitempty"`
	GatewayPublicKey  string             `json:"gatewayPublicKey,omitempty"`
	ForwarderPort     int32              `json:"forwarderPort,omitempty"`
	ForwarderEndpoint string             `json:"forwarderEndpoint,omitempty"`
	GatewayPodName    string             `json:"gatewayPodName,omitempty"`
	GatewayPodUID     string             `json:"gatewayPodUID,omitempty"`
	GatewayPodIP      string             `json:"gatewayPodIP,omitempty"`
	MappingRevision   int64              `json:"mappingRevision,omitempty"`
	Message           string             `json:"message,omitempty"`
	Conditions        []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type Tunnel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TunnelSpec   `json:"spec,omitempty"`
	Status TunnelStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

type TunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tunnel `json:"items"`
}
