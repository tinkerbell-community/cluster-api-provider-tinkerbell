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
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	rufiov1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	"k8s.io/utils/ptr"

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

func Test_taskBootOptions_explicit_are_copied_verbatim(t *testing.T) {
	t.Parallel()

	first := &tinkv2.BootOptions{ToggleNetboot: true, BootMode: tinkv2.BootModeNetboot}
	last := &tinkv2.BootOptions{
		BootMode: tinkv2.BootModeCustomboot,
		Customboot: &tinkv2.Customboot{
			PostOperations: []rufiov2.Operations{{PowerAction: ptr.To(rufiov2.PowerActionHardOff)}},
		},
	}

	tasks := []infrastructurev1.MachineTask{inlineTask("a"), refTask("b", "shared"), inlineTask("c")}
	tasks[0].BootOptions = first
	tasks[2].BootOptions = last

	for _, withBMC := range []bool{true, false} {
		g := NewWithT(t)

		hw := workflowTestHardware(withBMC)
		scope := workflowTestScope(g, "")
		// The machine's own boot options give way to the entries'.
		scope.tinkerbellMachine.Spec.BootOptions = infrastructurev1.BootOptions{BootMode: tinkv1.BootModeNetboot}

		got, err := scope.taskBootOptions(hw, tasks)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(HaveLen(3))
		g.Expect(cmp.Diff(first, got[0])).To(BeEmpty(), "with BMC: %v", withBMC)
		g.Expect(got[1]).To(BeNil(), "with BMC: %v", withBMC)
		g.Expect(cmp.Diff(last, got[2])).To(BeEmpty(), "with BMC: %v", withBMC)
		g.Expect(got[0]).NotTo(BeIdenticalTo(first), "the entries' boot options are copied")
	}
}

func Test_taskBootOptions_default_goes_on_the_first_entry(t *testing.T) {
	t.Parallel()

	tasks := []infrastructurev1.MachineTask{inlineTask("a"), refTask("b", "shared")}

	for withBMC, want := range map[bool]*tinkv2.BootOptions{
		true:  {ToggleNetboot: true, BootMode: tinkv2.BootModeNetboot},
		false: {ToggleNetboot: true},
	} {
		g := NewWithT(t)

		hw := workflowTestHardware(withBMC)
		scope := workflowTestScope(g, "")
		scope.tinkerbellMachine.Spec.BootOptions = infrastructurev1.BootOptions{BootMode: tinkv1.BootModeNetboot}

		got, err := scope.taskBootOptions(hw, tasks)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(HaveLen(2))
		g.Expect(got[0]).To(Equal(want), "with BMC: %v", withBMC)
		g.Expect(got[1]).To(BeNil(), "with BMC: %v", withBMC)
	}
}

func Test_taskBootOptions_default_iso(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(true)
	scope := workflowTestScope(g, "")
	scope.tinkerbellMachine.Spec.BootOptions = infrastructurev1.BootOptions{
		BootMode: tinkv1.BootModeISO,
		ISOURL:   "http://smee:7171/iso/hook.iso",
	}

	got, err := scope.taskBootOptions(hw, []infrastructurev1.MachineTask{inlineTask("a")})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal([]*tinkv2.BootOptions{{
		ToggleNetboot: true,
		BootMode:      tinkv2.BootModeIsoboot,
		ISOURL:        "http://smee:7171/iso/52-54-00-00-00-01/hook.iso",
	}}))
}

func Test_taskBootOptions_default_refuses_templated_customboot(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(true)
	scope := workflowTestScope(g, "")
	scope.tinkerbellMachine.Spec.BootOptions = infrastructurev1.BootOptions{
		BootMode: tinkv1.BootModeCustomboot,
		CustombootConfig: tinkv1.CustombootConfig{
			PreparingActions: []rufiov1.Action{{VirtualMediaAction: &rufiov1.VirtualMediaAction{
				MediaURL: "http://smee:7171/iso/{{ (index .Hardware.Interfaces 0).DHCP.MAC }}/hook.iso",
				Kind:     rufiov1.VirtualMediaCD,
			}}},
		},
	}

	_, err := scope.taskBootOptions(hw, []infrastructurev1.MachineTask{inlineTask("a")})
	g.Expect(errors.Is(err, ErrTemplatedCustombootConfig)).To(BeTrue(), "got %v", err)
}
