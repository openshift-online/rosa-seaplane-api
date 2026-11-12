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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ChannelNamespace holds region-local Channels configured by the operator's ConfigMap.
const ChannelNamespace = "channels"

// ChannelSpec defines the GitOps-managed configuration of an upgrade channel.
type ChannelSpec struct {
	// FeatureGates are the feature gates enabled for this channel.
	// +optional
	FeatureGates []string `json:"featureGates,omitempty"`

	// MinVersion is the minimum release version eligible for selection.
	// +optional
	MinVersion string `json:"minVersion,omitempty"`

	// ZOffset is the number of patch releases back from the latest available release.
	// +kubebuilder:validation:Minimum=0
	// +required
	ZOffset int32 `json:"zOffset"`

	// DayOffset is the minimum age in days of a release eligible for selection.
	// +kubebuilder:validation:Minimum=0
	// +required
	DayOffset int32 `json:"dayOffset"`
}

// ChannelRelease identifies a release and its payload image.
type ChannelRelease struct {
	// Version is the OpenShift release version.
	Version string `json:"version"`

	// Image is the release payload image.
	Image string `json:"image"`
}

// ChannelStatus holds the observed Cincinnati release snapshot for a channel.
type ChannelStatus struct {
	// Releases are the releases belonging to this channel in the Cincinnati graph.
	// +optional
	Releases []ChannelRelease `json:"releases,omitempty"`

	// Default is the selected default release, when one has been computed.
	// +optional
	Default *ChannelRelease `json:"default,omitempty"`

	// Conditions represent the latest observations of channel synchronization.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// Channel is the Schema for the channels API. Channel objects live in the channels namespace.
type Channel struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ChannelSpec `json:"spec"`

	// +optional
	Status ChannelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ChannelList contains a list of Channel.
type ChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Channel `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Channel{}, &ChannelList{})
		return nil
	})
}
