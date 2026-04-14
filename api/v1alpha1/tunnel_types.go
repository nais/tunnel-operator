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

type TunnelTarget struct {
	Host       string `json:"host"`
	Port       int32  `json:"port"`
	ResolvedIP string `json:"resolvedIP"`
}

type TunnelSpec struct {
	TeamSlug              string       `json:"teamSlug"`
	Environment           string       `json:"environment"`
	Target                TunnelTarget `json:"target"`
	ClientPublicKey       string       `json:"clientPublicKey"`
	ClientSTUNEndpoint    string       `json:"clientSTUNEndpoint,omitempty"`
	ActiveDeadlineSeconds *int64       `json:"activeDeadlineSeconds,omitempty"`
}

type TunnelStatus struct {
	Phase               TunnelPhase        `json:"phase,omitempty"`
	GatewayPublicKey    string             `json:"gatewayPublicKey,omitempty"`
	GatewaySTUNEndpoint string             `json:"gatewaySTUNEndpoint,omitempty"`
	GatewayPodName      string             `json:"gatewayPodName,omitempty"`
	Message             string             `json:"message,omitempty"`
	Conditions          []metav1.Condition `json:"conditions,omitempty"`
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

func init() {
	SchemeBuilder.Register(&Tunnel{}, &TunnelList{})
}
