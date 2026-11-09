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

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// inlineTask is a tasks entry with a one-action inline Task.
func inlineTask(name string) infrastructurev1.MachineTask {
	return infrastructurev1.MachineTask{
		Name:       name,
		TaskInline: &tinkv2.TaskSpec{Actions: []tinkv2.Action{{Name: name + "-action", Image: "alpine"}}},
	}
}

// refTask is a tasks entry referencing the Task named task.
func refTask(name, task string) infrastructurev1.MachineTask {
	return infrastructurev1.MachineTask{Name: name, TaskRef: &infrastructurev1.TaskRef{Name: task}}
}

func Test_hardwareTaskRefs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		annotation *string
		want       []string
	}{
		{name: "absent"},
		{name: "empty", annotation: new("")},
		{name: "one", annotation: new("a"), want: []string{"a"}},
		{name: "untidy", annotation: new(" a, ,b,"), want: []string{"a", "b"}},
		{name: "duplicates are kept", annotation: new("a,a"), want: []string{"a", "a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hw := workflowTestHardware(false)
			if tt.annotation != nil {
				hw.Annotations = map[string]string{HardwareTaskRefsAnnotation: *tt.annotation}
			}

			got := hardwareTaskRefs(hw)

			var names []string

			for _, task := range got {
				g.Expect(task.TaskRef).NotTo(BeNil())
				g.Expect(task.TaskInline).To(BeNil())
				g.Expect(task.TaskRef.Namespace).To(Equal(hw.Namespace))
				g.Expect(task.Name).To(Equal(task.TaskRef.Name))

				names = append(names, task.TaskRef.Name)
			}

			g.Expect(names).To(Equal(tt.want))
		})
	}
}

func Test_taskSource(t *testing.T) {
	t.Parallel()

	tasks := []infrastructurev1.MachineTask{inlineTask("a")}

	for _, tt := range []struct {
		name        string
		machine     infrastructurev1.TinkerbellMachineConfig
		annotations map[string]string
		cluster     infrastructurev1.TinkerbellClusterSpec
		wantLen     int
		wantOrigin  string
	}{
		{
			name:       "machine tasks beat cluster tasks",
			machine:    infrastructurev1.TinkerbellMachineConfig{Tasks: tasks},
			cluster:    infrastructurev1.TinkerbellClusterSpec{Tasks: []infrastructurev1.MachineTask{inlineTask("x"), inlineTask("y")}},
			wantLen:    1,
			wantOrigin: sourceMachine,
		},
		{
			name:    "machine templateInline beats cluster tasks",
			machine: infrastructurev1.TinkerbellMachineConfig{TemplateInline: "t"},
			cluster: infrastructurev1.TinkerbellClusterSpec{Tasks: tasks},
		},
		{
			name:    "machine templateRef beats cluster tasks",
			machine: infrastructurev1.TinkerbellMachineConfig{TemplateRef: &infrastructurev1.ObjectRef{Name: "t"}},
			cluster: infrastructurev1.TinkerbellClusterSpec{Tasks: tasks},
		},
		{
			name:        "hardware task annotation beats cluster templateInline",
			annotations: map[string]string{HardwareTaskRefsAnnotation: "shared"},
			cluster:     infrastructurev1.TinkerbellClusterSpec{TemplateInline: "t"},
			wantLen:     1,
			wantOrigin:  sourceHardware,
		},
		{
			name:        "hardware template annotation beats cluster tasks",
			annotations: map[string]string{HardwareTemplateOverrideAnnotation: "t"},
			cluster:     infrastructurev1.TinkerbellClusterSpec{Tasks: tasks},
		},
		{
			name:       "cluster tasks alone",
			cluster:    infrastructurev1.TinkerbellClusterSpec{Tasks: tasks},
			wantLen:    1,
			wantOrigin: sourceCluster,
		},
		{
			name: "nothing",
		},
		{
			name: "both hardware annotations: the task annotation wins",
			annotations: map[string]string{
				HardwareTaskRefsAnnotation:         "shared,other",
				HardwareTemplateOverrideAnnotation: "t",
			},
			wantLen:    2,
			wantOrigin: sourceHardware,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			scope := workflowTestScope(g, "")
			scope.tinkerbellMachine.Spec.TinkerbellMachineConfig = tt.machine
			scope.tinkerbellCluster.Spec = tt.cluster

			hw := workflowTestHardware(false)
			hw.Annotations = tt.annotations

			got, origin := scope.taskSource(hw)
			g.Expect(got).To(HaveLen(tt.wantLen))
			g.Expect(origin).To(Equal(tt.wantOrigin))
		})
	}
}
