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
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1beta1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta1"
	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// inline is a tasks entry named "a" with an inline Task.
func inline() infrastructurev1.MachineTask {
	return infrastructurev1.MachineTask{
		Name:       "a",
		TaskInline: &tinkv2.TaskSpec{Actions: []tinkv2.Action{{Name: "a", Image: "alpine"}}},
	}
}

func ref(name string) infrastructurev1.MachineTask {
	return infrastructurev1.MachineTask{Name: name, TaskRef: &infrastructurev1.TaskRef{Name: "shared"}}
}

func errorTypes(errs field.ErrorList) []field.ErrorType {
	types := make([]field.ErrorType, 0, len(errs))
	for _, err := range errs {
		types = append(types, err.Type)
	}

	return types
}

func Test_validateSource(t *testing.T) {
	t.Parallel()

	both := inline()
	both.TaskRef = &infrastructurev1.TaskRef{Name: "shared"}

	withRefs := inline()
	withRefs.TaskInline.References = map[string]tinkv2.Reference{"secret": {Name: "s", Resource: "secrets", Version: "v1"}}

	for _, tt := range []struct {
		name           string
		tasks          []infrastructurev1.MachineTask
		templateInline string
		templateRef    *infrastructurev1.ObjectRef
		want           []field.ErrorType
	}{
		{name: "no source"},
		{name: "tasks only", tasks: []infrastructurev1.MachineTask{inline(), ref("b")}},
		{name: "templateInline only", templateInline: "t"},
		{
			name:           "tasks and templateInline",
			tasks:          []infrastructurev1.MachineTask{inline()},
			templateInline: "t",
			want:           []field.ErrorType{field.ErrorTypeForbidden},
		},
		{
			name:        "tasks and templateRef",
			tasks:       []infrastructurev1.MachineTask{inline()},
			templateRef: &infrastructurev1.ObjectRef{Name: "t"},
			want:        []field.ErrorType{field.ErrorTypeForbidden},
		},
		{
			name:  "entry with both taskRef and taskInline",
			tasks: []infrastructurev1.MachineTask{both},
			want:  []field.ErrorType{field.ErrorTypeInvalid},
		},
		{
			name:  "entry with neither",
			tasks: []infrastructurev1.MachineTask{{Name: "a"}},
			want:  []field.ErrorType{field.ErrorTypeInvalid},
		},
		{
			// A Task's references read other objects with the access Tinkerbell's Policy gives
			// Tasks; CAPT must not create them for whoever can write a machine.
			name:  "inline task with references",
			tasks: []infrastructurev1.MachineTask{withRefs},
			want:  []field.ErrorType{field.ErrorTypeForbidden},
		},
		{
			name:  "reference without a name",
			tasks: []infrastructurev1.MachineTask{{Name: "a", TaskRef: &infrastructurev1.TaskRef{}}},
			want:  []field.ErrorType{field.ErrorTypeRequired},
		},
		{
			name:  "duplicate names",
			tasks: []infrastructurev1.MachineTask{inline(), ref("a")},
			want:  []field.ErrorType{field.ErrorTypeDuplicate},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			errs := validateSource(field.NewPath("spec"), tt.tasks, tt.templateInline, tt.templateRef)
			if len(tt.want) == 0 {
				g.Expect(errs).To(BeEmpty())

				return
			}

			g.Expect(errorTypes(errs)).To(Equal(tt.want), "%v", errs)
		})
	}
}

func Test_deprecationWarnings(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(deprecationWarnings(field.NewPath("spec"), "t", nil)).
		To(Equal(admission.Warnings{"spec.templateInline is deprecated; use spec.tasks"}))
	g.Expect(deprecationWarnings(field.NewPath("spec"), "", &infrastructurev1.ObjectRef{Name: "t"})).
		To(Equal(admission.Warnings{"spec.templateRef is deprecated; use spec.tasks"}))
	g.Expect(deprecationWarnings(field.NewPath("spec"), "", nil)).To(BeEmpty())
}

// createAll calls ValidateCreate on every webhook with a machine config and the same source
// fields on the cluster, keyed by webhook.
func createAll(config infrastructurev1.TinkerbellMachineConfig) map[string]func() (admission.Warnings, error) {
	cluster := infrastructurev1.TinkerbellClusterSpec{
		Tasks:          config.Tasks,
		TemplateInline: config.TemplateInline,
		TemplateRef:    config.TemplateRef,
	}

	return map[string]func() (admission.Warnings, error){
		"TinkerbellMachine": func() (admission.Warnings, error) {
			return (&TinkerbellMachine{}).ValidateCreate(context.Background(), &infrastructurev1.TinkerbellMachine{
				Spec: infrastructurev1.TinkerbellMachineSpec{TinkerbellMachineConfig: config},
			})
		},
		"TinkerbellMachineTemplate": func() (admission.Warnings, error) {
			tmt := &infrastructurev1.TinkerbellMachineTemplate{}
			tmt.Spec.Template.Spec = config

			return (&TinkerbellMachineTemplate{}).ValidateCreate(context.Background(), tmt)
		},
		"TinkerbellCluster": func() (admission.Warnings, error) {
			return (&TinkerbellCluster{}).ValidateCreate(context.Background(), &infrastructurev1.TinkerbellCluster{Spec: cluster})
		},
		"TinkerbellClusterTemplate": func() (admission.Warnings, error) {
			tct := &infrastructurev1.TinkerbellClusterTemplate{}
			tct.Spec.Template.Spec = cluster

			return (&TinkerbellClusterTemplate{}).ValidateCreate(context.Background(), tct)
		},
	}
}

func Test_webhooks_warn_on_deprecated_fields(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"TinkerbellMachine":         "spec.templateInline is deprecated; use spec.tasks",
		"TinkerbellMachineTemplate": "spec.template.spec.templateInline is deprecated; use spec.template.spec.tasks",
		"TinkerbellCluster":         "spec.templateInline is deprecated; use spec.tasks",
		"TinkerbellClusterTemplate": "spec.template.spec.templateInline is deprecated; use spec.template.spec.tasks",
	}

	for kind, create := range createAll(infrastructurev1.TinkerbellMachineConfig{TemplateInline: "t"}) {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			warnings, err := create()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(warnings).To(Equal(admission.Warnings{want[kind]}))
		})
	}
}

func Test_webhooks_reject_two_sources(t *testing.T) {
	t.Parallel()

	config := infrastructurev1.TinkerbellMachineConfig{
		Tasks:          []infrastructurev1.MachineTask{inline()},
		TemplateInline: "t",
	}

	for kind, create := range createAll(config) {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			_, err := create()
			g.Expect(err).To(MatchError(ContainSubstring("at most one of tasks, templateInline and templateRef may be set")))
		})
	}
}

// A v1beta1 client has no tasks field, but its write converts to a v1beta2 object that restores
// the tasks stashed in the conversion annotation: setting templateOverride then makes two
// sources, which the v1beta2 CEL rules never see on a v1beta1 write, so the webhook must.
func Test_webhook_rejects_a_v1beta1_write_restoring_tasks_next_to_a_template(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hub := &infrastructurev1.TinkerbellMachine{}
	hub.Spec.Tasks = []infrastructurev1.MachineTask{inline()}

	spoke := &infrav1beta1.TinkerbellMachine{}
	g.Expect(infrav1beta1.ConvertMachineFromHub(spoke, hub)).To(Succeed())

	spoke.Spec.TemplateOverride = "t"

	written := &infrastructurev1.TinkerbellMachine{}
	g.Expect(infrav1beta1.ConvertMachineToHub(spoke, written)).To(Succeed())
	g.Expect(written.Spec.Tasks).To(HaveLen(1), "the stashed tasks come back")

	_, err := (&TinkerbellMachine{}).ValidateCreate(context.Background(), written)
	g.Expect(err).To(MatchError(ContainSubstring("at most one of tasks, templateInline and templateRef may be set")))
}
