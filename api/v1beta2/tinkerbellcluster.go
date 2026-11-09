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

// ObjectRef is a reference to a Kubernetes object by name and namespace.
type ObjectRef struct {
	// Name is the name of the object.
	Name string `json:"name"`
	// Namespace is the namespace of the object.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

const (
	// ClusterFinalizer allows ReconcileTinkerbellCluster to clean up Tinkerbell resources before
	// removing it from the apiserver.
	ClusterFinalizer = "tinkerbellcluster.infrastructure.cluster.x-k8s.io"
)

// TinkerbellClusterSpec defines the desired state of TinkerbellCluster.
// +kubebuilder:validation:XValidation:rule="[has(self.tasks), has(self.templateInline), has(self.templateRef)].filter(x, x).size() <= 1",message="at most one of tasks, templateInline and templateRef may be set"
type TinkerbellClusterSpec struct {
	// ControlPlaneEndpoint is the address of the cluster control plane.
	// When not set, it is populated from the owning Cluster's spec.
	//
	// See https://cluster-api.sigs.k8s.io/developer/architecture/controllers/cluster.html
	// for more details.
	//
	// +optional
	ControlPlaneEndpoint *clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`

	// Tasks are the Tinkerbell v1alpha2 Tasks run in order to provision every machine in the
	// cluster, each given inline or by reference. A machine's own tasks or template, or its
	// Hardware's annotations, take precedence. Mutually exclusive with TemplateInline and
	// TemplateRef.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Tasks []MachineTask `json:"tasks,omitempty"`

	// TemplateInline is an inline Tinkerbell Template definition applied to all machines in the
	// cluster. If a Machine specifies its own template, the Machine's template takes precedence.
	// Mutually exclusive with Tasks and TemplateRef.
	// This field is deprecated; use tasks.
	//
	// +optional
	TemplateInline string `json:"templateInline,omitempty"`

	// TemplateRef is a reference to an existing Tinkerbell Template object whose spec.data will
	// be used as the template for all machines in the cluster. If a Machine specifies its own
	// template, the Machine's template takes precedence. Mutually exclusive with Tasks and
	// TemplateInline. When Namespace is omitted, defaults to the Hardware's namespace.
	// This field is deprecated; use tasks.
	//
	// +optional
	TemplateRef *ObjectRef `json:"templateRef,omitempty"`
}

// TinkerbellClusterStatus defines the observed state of TinkerbellCluster.
type TinkerbellClusterStatus struct {
	// Ready denotes that the cluster (infrastructure) is ready.
	// +optional
	Ready bool `json:"ready"`

	// Initialization provides observations of the TinkerbellCluster initialization process.
	// NOTE: Fields in this struct are part of the Cluster API contract and are used to orchestrate initial Cluster provisioning.
	// +optional
	Initialization *TinkerbellClusterInitializationStatus `json:"initialization,omitempty"`

	// Conditions defines current service state of the TinkerbellCluster.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TinkerbellClusterInitializationStatus provides observations of the TinkerbellCluster initialization process.
// +kubebuilder:validation:MinProperties=1
type TinkerbellClusterInitializationStatus struct {
	// provisioned is true when the infrastructure provider reports that the Cluster's infrastructure is fully provisioned.
	// NOTE: this field is part of the Cluster API contract, and it is used to orchestrate initial Cluster provisioning.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// +kubebuilder:subresource:status
// +kubebuilder:resource:path=tinkerbellclusters,scope=Namespaced,categories=cluster-api
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels.cluster\\.x-k8s\\.io/cluster-name",description="Cluster to which this TinkerbellCluster belongs"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready",description="TinkerbellCluster ready status"

// TinkerbellCluster is the Schema for the tinkerbellclusters API.
type TinkerbellCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TinkerbellClusterSpec   `json:"spec,omitempty"`
	Status TinkerbellClusterStatus `json:"status,omitempty"`
}

// GetConditions returns the conditions for the TinkerbellCluster.
func (c *TinkerbellCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the conditions on the TinkerbellCluster.
func (c *TinkerbellCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// TinkerbellClusterList contains a list of TinkerbellCluster.
type TinkerbellClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TinkerbellCluster `json:"items"`
}
