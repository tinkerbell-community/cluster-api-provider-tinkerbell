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

package webhooks //nolint:testpackage // tests unexported validation helpers

import (
	"context"
	"testing"

	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	infrav1beta1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta1"
	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// talosRef names a machine's TalosConfig with templates.
func talosRef() infrastructurev1.WorkflowReference {
	return infrastructurev1.WorkflowReference{
		Group:     "bootstrap.cluster.x-k8s.io",
		Version:   "v1beta1",
		Resource:  "talosconfigs",
		Name:      "{{ .bootstrap_config_name }}",
		Namespace: "{{ .machine_namespace }}",
	}
}

func Test_validateReferences(t *testing.T) {
	t.Parallel()

	with := func(change func(*infrastructurev1.WorkflowReference)) map[string]infrastructurev1.WorkflowReference {
		ref := talosRef()
		change(&ref)

		return map[string]infrastructurev1.WorkflowReference{"talos": ref}
	}

	valid := map[string]infrastructurev1.WorkflowReference{"talos": talosRef()}

	for _, tt := range []struct {
		name   string
		config infrastructurev1.TinkerbellMachineConfig
		want   []field.ErrorType
		detail string
	}{
		{name: "valid reference", config: infrastructurev1.TinkerbellMachineConfig{References: valid}},
		{
			name:   "template in group",
			config: infrastructurev1.TinkerbellMachineConfig{References: with(func(r *infrastructurev1.WorkflowReference) { r.Group = "{{ .cluster_name }}" })},
			want:   []field.ErrorType{field.ErrorTypeInvalid},
		},
		{
			name:   "unknown variable in name",
			config: infrastructurev1.TinkerbellMachineConfig{References: with(func(r *infrastructurev1.WorkflowReference) { r.Name = "{{ .machine_namespcae }}" })},
			want:   []field.ErrorType{field.ErrorTypeInvalid},
			detail: "machine_namespace",
		},
		{
			name:   "parse error in namespace",
			config: infrastructurev1.TinkerbellMachineConfig{References: with(func(r *infrastructurev1.WorkflowReference) { r.Namespace = "{{ .machine_namespace" })},
			want:   []field.ErrorType{field.ErrorTypeInvalid},
		},
		{
			name:   "literal namespace of another namespace",
			config: infrastructurev1.TinkerbellMachineConfig{References: with(func(r *infrastructurev1.WorkflowReference) { r.Namespace = "other" })},
			want:   []field.ErrorType{field.ErrorTypeForbidden},
			detail: "machine's namespace",
		},
		{
			name:   "literal namespace of the machine",
			config: infrastructurev1.TinkerbellMachineConfig{References: with(func(r *infrastructurev1.WorkflowReference) { r.Namespace = "capi" })},
		},
		{
			// Checked when the Workflow is created: the Hardware may share the machine's namespace.
			name:   "templated namespace of the Hardware",
			config: infrastructurev1.TinkerbellMachineConfig{References: with(func(r *infrastructurev1.WorkflowReference) { r.Namespace = "{{ .hardware_namespace }}" })},
		},
		{
			name:   "references with templateInline",
			config: infrastructurev1.TinkerbellMachineConfig{References: valid, TemplateInline: "t"},
			want:   []field.ErrorType{field.ErrorTypeForbidden},
			detail: "references and vars apply to tasks",
		},
		{
			name:   "vars only with templateRef",
			config: infrastructurev1.TinkerbellMachineConfig{Vars: map[string]string{"k": "v"}, TemplateRef: &infrastructurev1.ObjectRef{Name: "t"}},
			want:   []field.ErrorType{field.ErrorTypeForbidden},
			detail: "references and vars apply to tasks",
		},
		{name: "vars only", config: infrastructurev1.TinkerbellMachineConfig{Vars: map[string]string{"k": "{{ .references.x }}"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			errs := validateReferences(field.NewPath("spec"), "capi", &tt.config)
			if len(tt.want) == 0 {
				g.Expect(errs).To(BeEmpty())

				return
			}

			g.Expect(errorTypes(errs)).To(Equal(tt.want), "%v", errs)
			g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(tt.detail))
		})
	}
}

func Test_webhooks_check_references(t *testing.T) {
	t.Parallel()

	outside := talosRef()
	outside.Namespace = "other"
	refs := map[string]infrastructurev1.WorkflowReference{"talos": outside}

	t.Run("TinkerbellMachine", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		tm := &infrastructurev1.TinkerbellMachine{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "capi"}}
		tm.Spec.Tasks = []infrastructurev1.MachineTask{inline()}
		tm.Spec.References = refs

		_, err := (&TinkerbellMachine{}).ValidateCreate(context.Background(), tm)
		g.Expect(err).To(MatchError(And(ContainSubstring("spec.references[talos].namespace"), ContainSubstring("machine's namespace"))))
	})

	t.Run("TinkerbellMachineTemplate", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		tmt := &infrastructurev1.TinkerbellMachineTemplate{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "capi"}}
		tmt.Spec.Template.Spec.Tasks = []infrastructurev1.MachineTask{inline()}
		tmt.Spec.Template.Spec.References = refs

		_, err := (&TinkerbellMachineTemplate{}).ValidateCreate(context.Background(), tmt)
		g.Expect(err).To(MatchError(And(ContainSubstring("spec.template.spec.references[talos].namespace"), ContainSubstring("machine's namespace"))))
	})
}

// A v1beta1 client has no references field, but its write converts to a v1beta2 object that
// restores the references stashed in the conversion annotation: setting templateOverride then
// pairs them with a template, which the v1beta2 CEL rules never see on a v1beta1 write.
func Test_webhook_rejects_a_v1beta1_write_restoring_references_next_to_a_template(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hub := &infrastructurev1.TinkerbellMachine{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "capi"}}
	hub.Spec.References = map[string]infrastructurev1.WorkflowReference{"talos": talosRef()}

	spoke := &infrav1beta1.TinkerbellMachine{}
	g.Expect(infrav1beta1.ConvertMachineFromHub(spoke, hub)).To(Succeed())

	spoke.Spec.TemplateOverride = "t"

	written := &infrastructurev1.TinkerbellMachine{}
	g.Expect(infrav1beta1.ConvertMachineToHub(spoke, written)).To(Succeed())
	g.Expect(written.Spec.References).To(HaveLen(1), "the stashed references come back")

	_, err := (&TinkerbellMachine{}).ValidateCreate(context.Background(), written)
	g.Expect(err).To(MatchError(ContainSubstring("references and vars apply to tasks and cannot be set with templateInline or templateRef")))
}
