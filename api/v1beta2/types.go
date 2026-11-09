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
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// MachineTask is one Task the machine's Workflow runs, given inline or by reference, with the
// boot options applied around it, as in a task of a Tinkerbell v1alpha2 Workflow.
//
// +kubebuilder:validation:XValidation:rule="has(self.taskRef) != has(self.taskInline)",message="exactly one of taskRef and taskInline must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.taskInline) || !has(self.taskInline.references)",message="taskInline cannot set references; reference a Task you manage with taskRef instead"
type MachineTask struct {
	// Name identifies the entry. A Task CAPT creates from TaskInline is named
	// <machine name>-<name>.
	//
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// TaskRef names an existing Task. CAPT never creates, changes or deletes it.
	// Mutually exclusive with TaskInline.
	//
	// +optional
	TaskRef *TaskRef `json:"taskRef,omitempty"`

	// TaskInline is a Task definition. CAPT creates it as a Task owned by the machine and
	// deletes it with the machine. Its templates are rendered by Tinkerbell with v1alpha2
	// data, such as {{ (index .hardware.spec.storageDevices 0).name }}. It cannot set
	// references: those read other objects with the access Tinkerbell's Policy gives Tasks,
	// which CAPT does not grant to whoever can write a machine; put them in a Task you
	// manage and use TaskRef. Mutually exclusive with TaskRef.
	//
	// +optional
	TaskInline *tinkv2.TaskSpec `json:"taskInline,omitempty"`

	// BootOptions are applied around this Task, as in a task of a v1alpha2 Workflow: the
	// boot mode and netboot toggle before it, and netboot off, ISO eject and customboot post
	// operations after it succeeds. They are used as written: a BootMode is carried out
	// through the Hardware's BMC, so Tinkerbell fails the Workflow on Hardware without one.
	// When no entry of the list sets BootOptions, the first entry gets the machine's
	// spec.bootOptions, with netboot toggled.
	//
	// +optional
	BootOptions *tinkv2.BootOptions `json:"bootOptions,omitempty"`
}

// TaskRef references an existing Tinkerbell Task.
type TaskRef struct {
	// Name is the name of the Task.
	Name string `json:"name"`

	// Namespace is the namespace of the Task. Defaults to the Tinkerbell namespace, where
	// the machine's Workflow is created.
	//
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// TinkerbellResourceStatus describes the status of a Tinkerbell resource.
type TinkerbellResourceStatus int

// TinkerbellResourceStatus constants define the lifecycle states of a Tinkerbell resource.
const (
	TinkerbellResourceStatusPending TinkerbellResourceStatus = iota
	TinkerbellResourceStatusRunning
	TinkerbellResourceStatusFailed
	TinkerbellResourceStatusTimeout
	TinkerbellResourceStatusSuccess
)

// TinkerbellMachineTemplateResource describes the data needed to create a TinkerbellMachine
// from a template. It uses TinkerbellMachineConfig instead of TinkerbellMachineSpec because
// templates must not contain controller-managed runtime fields (HardwareName, ProviderID).
//
// The CAPI MachineDeployment controller copies spec.template.spec from the
// InfrastructureMachineTemplate into the new InfrastructureMachine.spec as raw JSON.
// Because TinkerbellMachineConfig is embedded (json:",inline") in TinkerbellMachineSpec,
// the JSON paths are identical — CAPI's copy produces a valid TinkerbellMachineSpec
// with HardwareName and ProviderID absent (zero-valued), which is correct since
// the controller sets them later during hardware selection.
type TinkerbellMachineTemplateResource struct {
	// Standard object's metadata applied to TinkerbellMachines created from this template.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// Spec is the specification of the desired behavior of the machine.
	Spec TinkerbellMachineConfig `json:"spec"`
}
