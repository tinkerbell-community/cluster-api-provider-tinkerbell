/*
Copyright The Tinkerbell Authors.

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

package machine //nolint:testpackage

import (
	"testing"

	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// talosReference names the machine's TalosConfig, in namespace.
func talosReference(namespace string) infrastructurev1.WorkflowReference {
	return infrastructurev1.WorkflowReference{
		Group:     "bootstrap.cluster.x-k8s.io",
		Version:   "v1beta1",
		Resource:  "talosconfigs",
		Name:      "{{ .bootstrap_config_name }}",
		Namespace: namespace,
	}
}

// talosVars carries the TalosConfig's schematic ID to the Tasks.
func talosVars() map[string]string {
	return map[string]string{"schematic_id": "{{ .references.talos.status.imageFactory.schematicID }}"}
}

// expectNothingCreated asserts the machine got no Workflow and no Tasks.
func expectNothingCreated(g Gomega, scope *machineReconcileScope) {
	err := scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected no Workflow, got %v", err)
	g.Expect(listTasks(g, scope)).To(BeEmpty(), "expected no Tasks")
}

func Test_createWorkflow_sets_references_vars_and_machine_variables(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("install")}
	scope.tinkerbellMachine.Spec.References = map[string]infrastructurev1.WorkflowReference{
		"talos": talosReference("{{ .machine_namespace }}"),
	}
	scope.tinkerbellMachine.Spec.Vars = talosVars()

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	wf := getWorkflow(g, scope)
	g.Expect(wf.Spec.References).To(Equal(map[string]tinkv2.Reference{"talos": {
		Group:     "bootstrap.cluster.x-k8s.io",
		Version:   "v1beta1",
		Resource:  "talosconfigs",
		Name:      "machine-1-config",
		Namespace: wfNamespace,
	}}))
	g.Expect(wf.Spec.Vars).To(Equal(talosVars()))
	g.Expect(wf.Spec.Globals.TemplateMap).To(Equal(map[string]string{
		"device_1":                wfAgent,
		"machine_name":            wfMachine,
		"machine_namespace":       wfNamespace,
		"cluster_name":            "cluster-1",
		"bootstrap_config_name":   "machine-1-config",
		"tinkerbell_machine_name": wfMachine,
		"hardware_name":           wfHardware,
		"hardware_namespace":      wfNamespace,
	}))
}

func Test_createWorkflow_without_references_sets_none(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("install")}

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	wf := getWorkflow(g, scope)
	g.Expect(wf.Spec.References).To(BeNil())
	g.Expect(wf.Spec.Vars).To(BeNil())
	g.Expect(wf.Spec.Globals.TemplateMap).To(HaveKeyWithValue("device_1", wfAgent))
}

// A reference that cannot be rendered stops the machine before anything is created, including
// the inline Tasks, which ensureTasks would otherwise create first.
func Test_createTaskWorkflow_reference_problems_create_nothing(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		ref    infrastructurev1.WorkflowReference
		setup  func(*machineReconcileScope)
		reason string
	}{
		{
			name: "empty bootstrap config name",
			ref:  talosReference(""),
			setup: func(s *machineReconcileScope) {
				s.machine.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{}
			},
			reason: "Warning ReferenceNotRendered",
		},
		{
			name: "name that is not a DNS-1123 subdomain",
			ref: infrastructurev1.WorkflowReference{
				Version: "v1", Resource: "configmaps", Name: "{{ .machine_name }}_x",
			},
			reason: "Warning ReferenceNotRendered",
		},
		{
			name: "unknown variable",
			ref: infrastructurev1.WorkflowReference{
				Version: "v1", Resource: "configmaps", Name: "{{ .nope }}",
			},
			reason: "Warning ReferenceNotRendered",
		},
		{
			name:   "another namespace",
			ref:    talosReference("other"),
			reason: "Warning ReferenceOutsideNamespace",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hw := workflowTestHardware(false)
			scope := workflowTestScope(g, "", hw)
			scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("install")}
			scope.tinkerbellMachine.Spec.References = map[string]infrastructurev1.WorkflowReference{"talos": tt.ref}

			if tt.setup != nil {
				tt.setup(scope)
			}

			err := scope.createWorkflow(hw)
			g.Expect(err).To(MatchError(ErrReferenceNotRendered))
			g.Expect(err.Error()).To(ContainSubstring("talos"))
			expectNothingCreated(g, scope)
			g.Expect(recordedEvents(scope)).To(ContainElement(ContainSubstring(tt.reason)))
		})
	}
}

// The Workflow lives in the Hardware's namespace, but a reference is limited to, and defaults
// to, the machine's namespace: the Tinkerbell reference policy cannot tell machines of
// different namespaces apart, so CAPT must.
func Test_references_use_the_machine_namespace_not_the_workflow_namespace(t *testing.T) {
	t.Parallel()

	inCAPINamespace := func(g Gomega) (*machineReconcileScope, *tinkv2.Hardware) {
		hw := workflowTestHardware(false)
		scope := workflowTestScope(g, "", hw)
		scope.tinkerbellMachine.Namespace = "capi"
		scope.machine.Namespace = "capi"
		scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("install")}

		return scope, hw
	}

	t.Run("an empty namespace is the machine's", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		scope, hw := inCAPINamespace(g)
		scope.tinkerbellMachine.Spec.References = map[string]infrastructurev1.WorkflowReference{"talos": talosReference("")}

		g.Expect(scope.createWorkflow(hw)).To(Succeed())
		g.Expect(getWorkflow(g, scope).Spec.References["talos"].Namespace).To(Equal("capi"))
	})

	t.Run("the Hardware's namespace is refused", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		scope, hw := inCAPINamespace(g)
		scope.tinkerbellMachine.Spec.References = map[string]infrastructurev1.WorkflowReference{
			"talos": talosReference("{{ .hardware_namespace }}"),
		}

		g.Expect(scope.createWorkflow(hw)).To(MatchError(ErrReferenceNotRendered))
		expectNothingCreated(g, scope)
		g.Expect(recordedEvents(scope)).To(ContainElement(ContainSubstring("Warning ReferenceOutsideNamespace")))
	})
}

// An external Tinkerbell would look the referenced objects up in its own cluster, where the
// machine's namespace means nothing.
func Test_createTaskWorkflow_external_tinkerbell(t *testing.T) {
	t.Parallel()

	t.Run("references are refused", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		hw := workflowTestHardware(false)
		scope := workflowTestScope(g, "", hw)
		scope.externalTinkerbell = true
		scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("install")}
		scope.tinkerbellMachine.Spec.References = map[string]infrastructurev1.WorkflowReference{"talos": talosReference("")}

		g.Expect(scope.createWorkflow(hw)).To(MatchError(ErrReferencesUnsupported))
		expectNothingCreated(g, scope)
		g.Expect(recordedEvents(scope)).To(ContainElement(ContainSubstring("Warning ReferencesUnsupported")))
	})

	t.Run("vars alone are kept", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		hw := workflowTestHardware(false)
		scope := workflowTestScope(g, "", hw)
		scope.externalTinkerbell = true
		scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("install")}
		scope.tinkerbellMachine.Spec.Vars = map[string]string{"disk": "{{ (index .hardware.spec.storageDevices 0).name }}"}

		g.Expect(scope.createWorkflow(hw)).To(Succeed())
		g.Expect(getWorkflow(g, scope).Spec.Vars).To(Equal(scope.tinkerbellMachine.Spec.Vars))
	})
}

// A template from the cluster cannot use the machine's references, which Tinkerbell ignores in
// v1alpha1 compatibility mode: the machine is provisioned exactly as before, and told why its
// references did nothing.
func Test_createWorkflow_template_source_ignores_references(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellCluster.Spec.TemplateInline = talosTemplate
	scope.tinkerbellMachine.Spec.References = map[string]infrastructurev1.WorkflowReference{"talos": talosReference("")}
	scope.tinkerbellMachine.Spec.Vars = talosVars()

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	wf := getWorkflow(g, scope)
	g.Expect(wf.Spec.References).To(BeEmpty())
	g.Expect(wf.Spec.Vars).To(BeEmpty())
	g.Expect(recordedEvents(scope)).To(ContainElement(ContainSubstring("Warning ReferencesIgnored")))
}
