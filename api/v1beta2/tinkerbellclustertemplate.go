/*
Copyright 2022 The Tinkerbell Authors.

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

package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// TinkerbellClusterTemplateSpec defines the desired state of TinkerbellClusterTemplate.
type TinkerbellClusterTemplateSpec struct {
	Template TinkerbellClusterTemplateResource `json:"template"`
}

// TinkerbellClusterTemplateResource describes the TinkerbellCluster stamped out from the template.
//
// The full TinkerbellClusterSpec is embedded: ControlPlaneEndpoint is optional and derived by the
// controller from the owning Cluster, so it is harmless (and conventionally present) in a template.
type TinkerbellClusterTemplateResource struct {
	// Standard object's metadata applied to TinkerbellClusters created from this template.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// Spec is the specification of the desired behavior of the cluster.
	Spec TinkerbellClusterSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=tinkerbellclustertemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion

// TinkerbellClusterTemplate is the Schema for the tinkerbellclustertemplates API.
// It is consumed by the Cluster API topology controller to stamp out TinkerbellClusters
// for ClusterClass-based clusters; CAPT itself does not reconcile it.
type TinkerbellClusterTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TinkerbellClusterTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// TinkerbellClusterTemplateList contains a list of TinkerbellClusterTemplate.
type TinkerbellClusterTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TinkerbellClusterTemplate `json:"items"`
}
