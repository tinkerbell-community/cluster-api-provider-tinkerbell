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
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	rufiov1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
			if len(tt.want) == 0 {
				g.Expect(got).To(BeEmpty())

				return
			}

			names := make([]string, 0, len(got))

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

// sharedTask is a Task the user manages, which CAPT only references.
func sharedTask(name, namespace string) *tinkv2.Task {
	return &tinkv2.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       tinkv2.TaskSpec{Actions: []tinkv2.Action{{Name: "shared", Image: "alpine"}}},
	}
}

// taskRefs returns the Task references of wf's tasks, in order.
func taskRefs(wf *tinkv2.Workflow) []tinkv2.SimpleReference {
	refs := make([]tinkv2.SimpleReference, 0, len(wf.Spec.Tasks))
	for _, task := range wf.Spec.Tasks {
		refs = append(refs, task.TaskRef)
	}

	return refs
}

func Test_createWorkflow_runs_inline_tasks(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("install")}

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	task := &tinkv2.Task{}
	g.Expect(scope.tinkerbellClient.Get(scope.ctx, types.NamespacedName{Name: "machine-1-install", Namespace: wfNamespace}, task)).To(Succeed())
	expectOwnedByMachine(g, scope, task)
	g.Expect(task.Spec).To(Equal(*inlineTask("install").TaskInline))

	wf := getWorkflow(g, scope)
	expectOwnedByMachine(g, scope, wf)
	g.Expect(wf.Annotations).NotTo(HaveKey(AnnotationCompatibilityMode))
	g.Expect(wf.Spec.Tasks).To(HaveLen(1))
	g.Expect(wf.Spec.Tasks[0].TaskRef).To(Equal(tinkv2.SimpleReference{Name: "machine-1-install", Namespace: wfNamespace}))
	g.Expect(wf.Spec.Tasks[0].AgentID).To(Equal(wfAgent))
	g.Expect(wf.Spec.Tasks[0].Hardware.HardwareRef).To(Equal(&tinkv2.SimpleReference{Name: wfHardware, Namespace: wfNamespace}))
	g.Expect(wf.Spec.Tasks[0].Hardware.BootOptions).To(Equal(&tinkv2.BootOptions{ToggleNetboot: true}))
	g.Expect(wf.Spec.Globals.TemplateMap).To(HaveKeyWithValue("device_1", wfAgent))

	exists, err := scope.templateExists()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exists).To(BeFalse(), "a machine running Tasks gets no v1alpha1 Template")
}

func Test_createWorkflow_runs_a_mixed_list_in_order(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	shared := sharedTask("shared", wfNamespace)
	scope := workflowTestScope(g, "", hw, shared)
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{
		inlineTask("a"), refTask("shared", "shared"), inlineTask("b"),
	}

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	g.Expect(taskRefs(getWorkflow(g, scope))).To(Equal([]tinkv2.SimpleReference{
		{Name: "machine-1-a", Namespace: wfNamespace},
		{Name: "shared", Namespace: wfNamespace},
		{Name: "machine-1-b", Namespace: wfNamespace},
	}))

	g.Expect(scope.removeDependencies()).To(Succeed())

	remaining := listTasks(g, scope)
	g.Expect(remaining).To(HaveLen(1), "the inline Tasks go with the machine")
	g.Expect(remaining[0].Name).To(Equal("shared"))
	g.Expect(remaining[0].Labels).To(BeEmpty(), "a referenced Task is never labelled")
	g.Expect(remaining[0].Spec).To(Equal(shared.Spec), "a referenced Task is never changed")
}

func Test_createTaskWorkflow_missing_ref_creates_nothing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a"), refTask("missing", "missing")}

	err := scope.createWorkflow(hw)
	g.Expect(errors.Is(err, ErrTaskNotFound)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(ContainSubstring("tink/missing"))

	g.Expect(listTasks(g, scope)).To(BeEmpty())
	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())
}

func Test_createTaskWorkflow_conflicting_task(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw, sharedTask("machine-1-a", wfNamespace))
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a")}

	err := scope.createWorkflow(hw)
	g.Expect(errors.Is(err, ErrTaskConflict)).To(BeTrue(), "got %v", err)
	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())
}

func Test_createWorkflow_tasks_ignore_a_stale_template(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	stale := &tinkv1.Template{
		ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace},
		Spec:       tinkv1.TemplateSpec{Data: ptr.To(talosTemplate)},
	}
	scope := workflowTestScope(g, "", hw, stale)
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a")}

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	wf := getWorkflow(g, scope)
	g.Expect(wf.Annotations).NotTo(HaveKey(AnnotationCompatibilityMode))
	g.Expect(taskRefs(wf)).To(Equal([]tinkv2.SimpleReference{{Name: "machine-1-a", Namespace: wfNamespace}}))
}

func Test_createWorkflow_tasks_from_the_cluster_and_annotation(t *testing.T) {
	t.Parallel()

	t.Run("cluster", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		hw := workflowTestHardware(false)
		scope := workflowTestScope(g, "", hw)
		scope.tinkerbellCluster.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a")}

		g.Expect(scope.createWorkflow(hw)).To(Succeed())
		g.Expect(taskRefs(getWorkflow(g, scope))).To(Equal([]tinkv2.SimpleReference{{Name: "machine-1-a", Namespace: wfNamespace}}))
	})

	t.Run("hardware annotation", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		hw := workflowTestHardware(true)
		hw.Annotations = map[string]string{HardwareTaskRefsAnnotation: "shared"}
		scope := workflowTestScope(g, "", hw, sharedTask("shared", wfNamespace))
		scope.tinkerbellMachine.Spec.BootOptions = infrastructurev1.BootOptions{BootMode: tinkv1.BootModeNetboot}

		g.Expect(scope.createWorkflow(hw)).To(Succeed())

		wf := getWorkflow(g, scope)
		g.Expect(taskRefs(wf)).To(Equal([]tinkv2.SimpleReference{{Name: "shared", Namespace: wfNamespace}}))
		g.Expect(wf.Spec.Tasks[0].Hardware.BootOptions).To(Equal(&tinkv2.BootOptions{ToggleNetboot: true, BootMode: tinkv2.BootModeNetboot}))
	})
}

func Test_createTaskWorkflow_uses_the_tinkerbell_client(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "")
	scope.tinkerbellClient = newHardwareTestClient(g, hw, sharedTask("shared", wfNamespace))
	scope.externalTinkerbell = true
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a"), refTask("shared", "shared")}

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	g.Expect(getWorkflow(g, scope).Spec.Tasks).To(HaveLen(2))
	g.Expect(listTasks(g, scope)).To(HaveLen(2))

	managementTasks := &tinkv2.TaskList{}
	g.Expect(scope.client.List(scope.ctx, managementTasks)).To(Succeed())
	g.Expect(managementTasks.Items).To(BeEmpty())
	g.Expect(apierrors.IsNotFound(scope.client.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())
}

func Test_reconcile_reports_a_workflow_that_does_not_render(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	wf := &tinkv2.Workflow{ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace}}
	wf.Status.SetCondition(tinkv2.WorkflowCondition{
		Type:    tinkv2.TemplateRenderedSuccess,
		Status:  metav1.ConditionFalse,
		Message: "boom",
	})
	scope := workflowTestScope(g, "", hw, wf)

	err := scope.reconcile(hw)
	g.Expect(errors.Is(err, errWorkflowNotRendered)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(ContainSubstring("boom"))
}

// An entry's bootMode needs a BMC: Tinkerbell fails the Workflow on Hardware without one, so
// CAPT names those entries for a warning.
func Test_bootModesWithoutBMC(t *testing.T) {
	t.Parallel()

	tasks := []infrastructurev1.MachineTask{inlineTask("netboot"), inlineTask("toggle"), refTask("plain", "shared")}
	tasks[0].BootOptions = &tinkv2.BootOptions{ToggleNetboot: true, BootMode: tinkv2.BootModeNetboot}
	tasks[1].BootOptions = &tinkv2.BootOptions{ToggleNetboot: true}

	g := NewWithT(t)
	g.Expect(bootModesWithoutBMC(workflowTestHardware(false), tasks)).To(Equal([]string{"netboot"}))
	g.Expect(bootModesWithoutBMC(workflowTestHardware(true), tasks)).To(BeEmpty())
}

// recordedEvents drains the events the scope's fake recorder holds, as "Type Reason message".
func recordedEvents(scope *machineReconcileScope) []string {
	rec, ok := scope.recorder.(*record.FakeRecorder)
	if !ok {
		return nil
	}

	var out []string

	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// Each problem a Task source can have is recorded on the TinkerbellMachine, where the user
// looks, and not only in the controller's logs.
func Test_task_source_problems_are_recorded_on_the_machine(t *testing.T) {
	t.Parallel()

	withRefs := func(scope *machineReconcileScope) {
		scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{refTask("missing", "missing")}
	}

	for _, tt := range []struct {
		name    string
		bmc     bool
		objects []client.Object
		setup   func(*machineReconcileScope, *tinkv2.Hardware)
		want    string
	}{
		{
			name:  "missing reference",
			setup: func(s *machineReconcileScope, _ *tinkv2.Hardware) { withRefs(s) },
			want:  "Warning TaskNotFound",
		},
		{
			name:    "taken inline name",
			objects: []client.Object{sharedTask("machine-1-a", wfNamespace)},
			setup: func(s *machineReconcileScope, _ *tinkv2.Hardware) {
				s.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a")}
			},
			want: "Warning TaskConflict",
		},
		{
			name: "machine boot options ignored",
			bmc:  true,
			setup: func(s *machineReconcileScope, _ *tinkv2.Hardware) {
				s.tinkerbellMachine.Spec.BootOptions = infrastructurev1.BootOptions{BootMode: tinkv1.BootModeNetboot}
				s.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a")}
				s.tinkerbellMachine.Spec.Tasks[0].BootOptions = &tinkv2.BootOptions{ToggleNetboot: true}
			},
			want: "Warning BootOptionsIgnored",
		},
		{
			name: "boot mode without a BMC",
			setup: func(s *machineReconcileScope, _ *tinkv2.Hardware) {
				s.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a")}
				s.tinkerbellMachine.Spec.Tasks[0].BootOptions = &tinkv2.BootOptions{BootMode: tinkv2.BootModeNetboot}
			},
			want: "Warning BootModeWithoutBMC",
		},
		{
			name: "templated customboot",
			bmc:  true,
			setup: func(s *machineReconcileScope, _ *tinkv2.Hardware) {
				s.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a")}
				s.tinkerbellMachine.Spec.BootOptions = infrastructurev1.BootOptions{
					BootMode: tinkv1.BootModeCustomboot,
					CustombootConfig: tinkv1.CustombootConfig{PreparingActions: []rufiov1.Action{{VirtualMediaAction: &rufiov1.VirtualMediaAction{
						MediaURL: "http://smee/{{ .Hardware.Disks }}.iso", Kind: rufiov1.VirtualMediaCD,
					}}}},
				}
			},
			want: "Warning TemplatedCustombootConfig",
		},
		{
			name:    "both hardware annotations",
			objects: []client.Object{sharedTask("shared", wfNamespace)},
			setup: func(_ *machineReconcileScope, hw *tinkv2.Hardware) {
				hw.Annotations = map[string]string{HardwareTaskRefsAnnotation: "shared", HardwareTemplateOverrideAnnotation: "t"}
			},
			want: "Warning ConflictingHardwareAnnotations",
		},
		{
			name: "deprecated template annotation",
			objects: []client.Object{&tinkv1.Template{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: wfNamespace},
				Spec:       tinkv1.TemplateSpec{Data: ptr.To(talosTemplate)},
			}},
			setup: func(_ *machineReconcileScope, hw *tinkv2.Hardware) {
				hw.Annotations = map[string]string{HardwareTemplateOverrideAnnotation: "t"}
			},
			want: "Normal DeprecatedTemplateSource",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hw := workflowTestHardware(tt.bmc)
			scope := workflowTestScope(g, "", append(tt.objects, hw)...)
			tt.setup(scope, hw)

			_ = scope.createWorkflow(hw)
			g.Expect(recordedEvents(scope)).To(ContainElement(HavePrefix(tt.want)))
		})
	}
}

func Test_reconcile_records_a_workflow_that_does_not_render(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	wf := &tinkv2.Workflow{ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace}}
	wf.Status.SetCondition(tinkv2.WorkflowCondition{Type: tinkv2.TemplateRenderedSuccess, Status: metav1.ConditionFalse, Message: "boom"})
	scope := workflowTestScope(g, "", hw, wf)

	g.Expect(scope.reconcile(hw)).NotTo(Succeed())
	g.Expect(recordedEvents(scope)).To(ContainElement(SatisfyAll(HavePrefix("Warning WorkflowNotRendered"), ContainSubstring("boom"))))
}

func Test_createTaskWorkflow_records_a_task_it_cannot_read(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellClient = fake.NewClientBuilder().WithScheme(hardwareTestScheme(g)).WithObjects(hw).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*tinkv2.Task); ok && key.Name == "shared" {
					return apierrors.NewForbidden(tinkv2.GroupVersion.WithResource("tasks").GroupResource(), key.Name, errors.New("no access"))
				}

				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{refTask("shared", "shared")}

	err := scope.createWorkflow(hw)
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
	g.Expect(recordedEvents(scope)).To(ContainElement(SatisfyAll(HavePrefix("Warning TaskNotReadable"), ContainSubstring("tink/shared"))))
}

func Test_createTaskWorkflow_reports_every_missing_ref(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{
		refTask("first", "missing-a"), inlineTask("x"), refTask("second", "missing-b"),
	}

	err := scope.createWorkflow(hw)
	g.Expect(errors.Is(err, ErrTaskNotFound)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(SatisfyAll(ContainSubstring("tink/missing-a"), ContainSubstring("tink/missing-b")))

	var notFound []string

	for _, e := range recordedEvents(scope) {
		if strings.HasPrefix(e, "Warning TaskNotFound") {
			notFound = append(notFound, e)
		}
	}

	g.Expect(notFound).To(HaveLen(2))
	g.Expect(listTasks(g, scope)).To(BeEmpty())
}

// A Task CAPT created for a machine is deleted with that machine, and with this machine's own
// cleanup, so a taskRef to one is refused: CAPT never deletes a Task someone references.
func Test_createTaskWorkflow_refuses_a_reference_to_a_machine_task(t *testing.T) {
	t.Parallel()

	for _, owner := range []string{wfMachine, "machine-2"} {
		t.Run(owner, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			managed := sharedTask(owner+"-old", wfNamespace)
			managed.Labels = map[string]string{LabelMachineName: owner, LabelMachineNamespace: wfNamespace}

			hw := workflowTestHardware(false)
			scope := workflowTestScope(g, "", hw, managed)
			scope.tinkerbellMachine.Spec.Tasks = []infrastructurev1.MachineTask{inlineTask("a"), refTask("old", managed.Name)}

			err := scope.createWorkflow(hw)
			g.Expect(errors.Is(err, ErrTaskManagedByCAPT)).To(BeTrue(), "got %v", err)
			g.Expect(recordedEvents(scope)).To(ContainElement(HavePrefix("Warning TaskManagedByCAPT")))

			remaining := listTasks(g, scope)
			g.Expect(remaining).To(HaveLen(1), "the referenced Task stays, and nothing is created")
			g.Expect(remaining[0].Name).To(Equal(managed.Name))
			g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())
		})
	}
}
