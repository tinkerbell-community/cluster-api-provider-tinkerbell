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

package machine //nolint:testpackage

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	"github.com/tinkerbell/tinkerbell/api/conversion"
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

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

const (
	wfMachine   = "machine-1"
	wfNamespace = "tink"
	wfHardware  = "hw-1"
	wfAgent     = "52:54:00:00:00:01"

	// talosTemplate is a CAPT-style template: Talos streamed to the first disk with its machine
	// configuration as user data, then a reboot. It converts to v1alpha2.
	talosTemplate = `version: "0.1"
name: talos
global_timeout: 9800
tasks:
  - name: "talos"
    worker: "{{.device_1}}"
    volumes:
      - /dev:/dev
      - /dev/console:/dev/console
      - /lib/firmware:/lib/firmware:ro
    actions:
      - name: "stream-image"
        image: quay.io/tinkerbell/actions/image2disk:latest
        timeout: 600
        environment:
          IMG_URL: "http://10.1.1.11:8080/metal-amd64.raw.zst"
          DEST_DISK: "{{ index .Hardware.Disks 0 }}"
          COMPRESSED: true
      - name: "write-config"
        image: ghcr.io/sidero-community/actions/talos2disk:latest
        timeout: 300
        environment:
          DEST_DISK: "{{ index .Hardware.Disks 0 }}"
          USER_DATA: "{{ .Hardware.UserData }}"
          WORKER: "{{ .device_1 }}"
      - name: "reboot"
        image: ghcr.io/jacobweinstock/waitdaemon:latest
        timeout: 90
        pid: host
        command: ["reboot"]
        environment:
          IMAGE: alpine
          WAIT_SECONDS: 10
        volumes:
          - /var/run/docker.sock:/var/run/docker.sock
`

	// rangeTemplate writes one action per disk: its structure depends on templating, so it has
	// no v1alpha2 translation.
	rangeTemplate = `version: "0.1"
name: wipe
global_timeout: 600
tasks:
  - name: "wipe"
    worker: "{{.device_1}}"
    actions:
{{- range $i, $disk := .Hardware.Disks }}
      - name: "wipe-{{ $i }}"
        image: quay.io/tinkerbell/actions/wipe:latest
        timeout: 90
        environment:
          DEST_DISK: "{{ $disk }}"
{{- end }}
`

	// partitionTemplate writes to a partition of the first disk, nesting the disk index in
	// formatPartition as CAPT's Quick Start template does.
	partitionTemplate = `version: "0.1"
name: partition
global_timeout: 600
tasks:
  - name: "write"
    worker: "{{.device_1}}"
    actions:
      - name: "write-file"
        image: quay.io/tinkerbell/actions/writefile:latest
        timeout: 90
        environment:
          DEST_DISK: "{{ formatPartition ( index .Hardware.Disks 0 ) 3 }}"
`

	// twoTaskTemplate converts, but v1alpha2 applies boot options per task, and CAPT's Workflow
	// always has boot options (it toggles netboot).
	twoTaskTemplate = `version: "0.1"
name: two
global_timeout: 600
tasks:
  - name: "first"
    worker: "{{.device_1}}"
    actions:
      - name: "a"
        image: alpine
        timeout: 60
  - name: "second"
    worker: "{{.device_1}}"
    actions:
      - name: "b"
        image: alpine
        timeout: 60
`
)

func workflowTestHardware(withBMC bool) *tinkv2.Hardware {
	hw := &tinkv2.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: wfHardware, Namespace: wfNamespace},
		Spec: tinkv2.HardwareSpec{
			StorageDevices:    []tinkv2.StorageDevice{{Name: "/dev/sda"}},
			Instance:          &tinkv2.Instance{LookupID: wfAgent},
			NetworkInterfaces: tinkv2.NetworkInterfaces{wfAgent: {}},
		},
	}

	if withBMC {
		hw.Spec.BMC = &tinkv2.BMC{Connection: &rufiov2.Connection{Host: "10.0.0.10"}}
	}

	return hw
}

// workflowTestScope returns a scope for a machine whose template is data, in the namespace of
// its Hardware (so CAPT sets owner references, as it does locally in the same namespace).
func workflowTestScope(g Gomega, data string, objects ...client.Object) *machineReconcileScope {
	machine := &infrastructurev1.TinkerbellMachine{
		ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace, UID: "machine-1-uid"},
		Status:     infrastructurev1.TinkerbellMachineStatus{TargetNamespace: wfNamespace},
	}
	machine.Spec.TemplateInline = data

	cl := newHardwareTestClient(g, objects...)

	return &machineReconcileScope{
		ctx:               context.Background(),
		log:               logr.Discard(),
		client:            cl,
		tinkerbellClient:  cl,
		scheme:            hardwareTestScheme(g),
		tinkerbellMachine: machine,
		tinkerbellCluster: &infrastructurev1.TinkerbellCluster{},
		recorder:          record.NewFakeRecorder(100),
	}
}

func machineKey() types.NamespacedName {
	return types.NamespacedName{Name: wfMachine, Namespace: wfNamespace}
}

func getWorkflow(g Gomega, scope *machineReconcileScope) *tinkv2.Workflow {
	wf := &tinkv2.Workflow{}
	g.Expect(scope.tinkerbellClient.Get(scope.ctx, machineKey(), wf)).To(Succeed(), "Expected the Workflow to be created")

	return wf
}

func listTasks(g Gomega, scope *machineReconcileScope) []tinkv2.Task {
	tasks := &tinkv2.TaskList{}
	g.Expect(scope.tinkerbellClient.List(scope.ctx, tasks, client.InNamespace(wfNamespace))).To(Succeed())

	return tasks.Items
}

// expectOwnedByMachine asserts obj is owned by the machine as every object CAPT creates for it.
func expectOwnedByMachine(g Gomega, scope *machineReconcileScope, obj client.Object) {
	g.Expect(obj.GetLabels()).To(HaveKeyWithValue(LabelMachineName, wfMachine))
	g.Expect(obj.GetLabels()).To(HaveKeyWithValue(LabelMachineNamespace, wfNamespace))
	g.Expect(metav1.IsControlledBy(obj, scope.tinkerbellMachine)).To(BeTrue(), "Expected the machine to control %s", obj.GetName())
}

// expectV1Alpha1View asserts that a v1alpha1 client reads wf as the Workflow CAPT created
// before it moved to v1alpha2.
func expectV1Alpha1View(g Gomega, scope *machineReconcileScope, hw *tinkv2.Hardware, wf *tinkv2.Workflow) {
	want, err := scope.v1alpha1Workflow(hw)
	g.Expect(err).NotTo(HaveOccurred())

	// v1alpha2 has a single boot mode for ISO boot; the deprecated "iso" reads back as "isoboot".
	if want.Spec.BootOptions.BootMode == tinkv1.BootModeISO {
		want.Spec.BootOptions.BootMode = tinkv1.BootModeIsoboot
	}

	view := &tinkv1.Workflow{}
	g.Expect(conversion.WorkflowToV1Alpha1(wf, view)).To(Succeed())
	g.Expect(view.Spec).To(Equal(want.Spec), "v1alpha1 view (-want +got):\n%s", cmp.Diff(want.Spec, view.Spec))
	g.Expect(view.Labels).To(Equal(want.Labels))
	g.Expect(view.OwnerReferences).To(Equal(want.OwnerReferences))
}

// Test_workflowTemplateData asserts the Workflow's hardwareMap carries the hardware
// identifier and nothing else. Which operating system image a machine gets is not the
// infrastructure provider's concern: a template reads that from the Hardware object itself.
func Test_workflowTemplateData(t *testing.T) {
	t.Parallel()

	scope := &machineReconcileScope{
		tinkerbellMachine: &infrastructurev1.TinkerbellMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "myMachine", Namespace: "myNamespace"},
		},
	}

	cases := map[string]struct {
		spec tinkv2.HardwareSpec
		want string
	}{
		"the_lookup_id_where_v1alpha1_metadata_instance_id_converts_to": {
			spec: tinkv2.HardwareSpec{
				AgentID:  "52:54:00:00:00:01",
				Instance: &tinkv2.Instance{LookupID: "de:ad:be:ef:00:01"},
			},
			want: "de:ad:be:ef:00:01",
		},
		"the_agent_id_when_there_is_no_lookup_id": {
			spec: tinkv2.HardwareSpec{AgentID: "52:54:00:00:00:01", Instance: &tinkv2.Instance{}},
			want: "52:54:00:00:00:01",
		},
		"the_agent_id_when_there_is_no_instance": {
			spec: tinkv2.HardwareSpec{AgentID: "52:54:00:00:00:01"},
			want: "52:54:00:00:00:01",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hw := &tinkv2.Hardware{
				ObjectMeta: metav1.ObjectMeta{Name: "myHardware", Namespace: "myNamespace"},
				Spec:       tc.spec,
			}

			g.Expect(scope.workflowTemplateData(hw)).To(Equal(map[string]string{"device_1": tc.want}))
		})
	}
}

// A template that converts runs natively: CAPT creates its Tasks, owned by the machine, and a
// Workflow that runs them, and no v1alpha1 Template.
func Test_createWorkflow_runs_a_template_that_converts_as_tasks(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, talosTemplate, hw)

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv1.Template{}))).To(BeTrue(),
		"no v1alpha1 Template for a template that converts")

	conv, err := conversion.ConvertTemplate(talosTemplate, templateFuncs())
	g.Expect(err).NotTo(HaveOccurred())

	want := conversion.TemplateTasks(scope.templateObject(""), conv)
	g.Expect(want).To(HaveLen(1))

	tasks := listTasks(g, scope)
	g.Expect(tasks).To(HaveLen(1))

	task := tasks[0]
	g.Expect(task.Name).To(Equal(wfMachine+"-talos"), "Tasks are named <template>-<task>, the template being the machine's")
	g.Expect(task.Spec).To(Equal(want[0].Spec), "task spec (-want +got):\n%s", cmp.Diff(want[0].Spec, task.Spec))
	g.Expect(task.Annotations).To(HaveKeyWithValue(conversion.AnnotationTemplate, wfMachine))
	g.Expect(task.Annotations).To(HaveKeyWithValue(conversion.AnnotationTemplateTask, "talos"),
		"Tinkerbell reports the rendered task under its template task name")
	expectOwnedByMachine(g, scope, &task)

	// The actions read the v1alpha2 Hardware and the Workflow's Agent instead of v1alpha1 data.
	g.Expect(task.Spec.Actions).To(HaveLen(3))
	g.Expect(task.Spec.Actions[0].Env).To(HaveKeyWithValue("DEST_DISK", "{{(index .hardware.spec.storageDevices 0).name}}"))
	g.Expect(task.Spec.Actions[1].Env).To(HaveKeyWithValue("WORKER", "{{(index .workflow.spec.tasks 0).agentID}}"))
	g.Expect(task.Spec.Actions[2].Command).To(Equal("reboot"))
	g.Expect(task.Spec.Actions[2].Namespaces.PID).To(Equal("host"))

	wf := getWorkflow(g, scope)
	g.Expect(wf.Annotations).To(HaveKeyWithValue(conversion.AnnotationTemplate, wfMachine))
	g.Expect(wf.Annotations).NotTo(HaveKey(conversion.AnnotationV1Alpha1), "the conversion annotation is the webhook's")
	g.Expect(wf.Annotations).NotTo(HaveKey(AnnotationCompatibilityMode))
	g.Expect(wf.Spec).To(Equal(tinkv2.WorkflowSpec{
		Globals:        &tinkv2.Extra{TemplateMap: map[string]string{"device_1": wfAgent}},
		TimeoutSeconds: ptr.To[int64](9800),
		Tasks: []tinkv2.WorkflowTask{{
			AgentID: wfAgent,
			TaskRef: tinkv2.SimpleReference{Name: wfMachine + "-talos", Namespace: wfNamespace},
			Hardware: &tinkv2.WorkflowHardware{
				HardwareRef: &tinkv2.SimpleReference{Name: wfHardware, Namespace: wfNamespace},
				BootOptions: &tinkv2.BootOptions{ToggleNetboot: true},
			},
		}},
	}))
	expectOwnedByMachine(g, scope, wf)
	expectV1Alpha1View(g, scope, hw, wf)
}

// Boot options carry over to the native Workflow, and a v1alpha1 client still reads the
// Workflow CAPT created before.
func Test_createWorkflow_keeps_the_boot_options(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		options infrastructurev1.BootOptions
		want    *tinkv2.BootOptions
	}{
		"netboot": {
			options: infrastructurev1.BootOptions{BootMode: tinkv1.BootModeNetboot},
			want:    &tinkv2.BootOptions{ToggleNetboot: true, BootMode: tinkv2.BootModeNetboot},
		},
		"isoboot": {
			options: infrastructurev1.BootOptions{BootMode: tinkv1.BootModeIsoboot, ISOURL: "http://smee:7171/iso/hook.iso"},
			want: &tinkv2.BootOptions{
				ToggleNetboot: true, BootMode: tinkv2.BootModeIsoboot,
				ISOURL: "http://smee:7171/iso/52-54-00-00-00-01/hook.iso",
			},
		},
		"deprecated_iso": {
			options: infrastructurev1.BootOptions{BootMode: tinkv1.BootModeISO, ISOURL: "http://smee:7171/iso/hook.iso"},
			want: &tinkv2.BootOptions{
				ToggleNetboot: true, BootMode: tinkv2.BootModeIsoboot,
				ISOURL: "http://smee:7171/iso/52-54-00-00-00-01/hook.iso",
			},
		},
		"customboot": {
			options: infrastructurev1.BootOptions{
				BootMode: tinkv1.BootModeCustomboot,
				CustombootConfig: tinkv1.CustombootConfig{
					PreparingActions: []rufiov1.Action{
						{PowerAction: ptr.To(rufiov1.PowerHardOff)},
						{BootDevice: &rufiov1.BootDeviceConfig{Device: rufiov1.PXE, EFIBoot: true}},
						{PowerAction: ptr.To(rufiov1.PowerOn)},
					},
					PostActions: []rufiov1.Action{{PowerAction: ptr.To(rufiov1.PowerHardOff)}},
				},
			},
			want: &tinkv2.BootOptions{
				ToggleNetboot: true, BootMode: tinkv2.BootModeCustomboot,
				Customboot: &tinkv2.Customboot{
					PreOperations: []rufiov2.Operations{
						{PowerAction: ptr.To(rufiov2.PowerActionHardOff)},
						{BootDevice: &rufiov2.BootDeviceConfig{Name: rufiov2.BootDevicePXE, EFIBoot: true}},
						{PowerAction: ptr.To(rufiov2.PowerActionOn)},
					},
					PostOperations: []rufiov2.Operations{{PowerAction: ptr.To(rufiov2.PowerActionHardOff)}},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hw := workflowTestHardware(true)
			scope := workflowTestScope(g, talosTemplate, hw)
			scope.tinkerbellMachine.Spec.BootOptions = tc.options

			g.Expect(scope.createWorkflow(hw)).To(Succeed())

			wf := getWorkflow(g, scope)
			g.Expect(wf.Annotations).NotTo(HaveKey(AnnotationCompatibilityMode), "a single-task template runs natively with boot options")
			g.Expect(wf.Spec.Tasks).To(HaveLen(1))
			g.Expect(wf.Spec.Tasks[0].Hardware.BootOptions).To(Equal(tc.want))
			expectV1Alpha1View(g, scope, hw, wf)
		})
	}
}

// A template that does not convert, or a Workflow that cannot run its Tasks, runs in v1alpha1
// compatibility mode: CAPT creates the v1alpha1 Template, as before, and the converted Workflow,
// and records why.
func Test_createWorkflow_falls_back_to_the_v1alpha1_template(t *testing.T) {
	t.Parallel()

	customboot := infrastructurev1.BootOptions{
		BootMode: tinkv1.BootModeCustomboot,
		CustombootConfig: tinkv1.CustombootConfig{
			PreparingActions: []rufiov1.Action{{VirtualMediaAction: &rufiov1.VirtualMediaAction{
				MediaURL: "http://smee:7171/iso/{{ (index .Hardware.Interfaces 0).DHCP.MAC }}/hook.iso",
				Kind:     rufiov1.VirtualMediaCD,
			}}},
		},
	}

	cases := map[string]struct {
		data       string
		options    infrastructurev1.BootOptions
		wantReason string
	}{
		"a_range_over_the_disks": {
			data:       rangeTemplate,
			wantReason: `the template does not convert to v1alpha2 Tasks: it uses "range"`,
		},
		"an_interface_of_the_hardware": {
			data:       strings.Replace(partitionTemplate, "( index .Hardware.Disks 0 ) 3", "( index .Hardware.Interfaces 0 ).DHCP.IfaceName 3", 1),
			wantReason: "the template does not convert to v1alpha2 Tasks: it uses .Hardware.Interfaces",
		},
		"several_tasks_with_boot_options": {
			data:       twoTaskTemplate,
			wantReason: "the Workflow cannot run the template's Tasks",
		},
		"templated_customboot_operations": {
			data:       talosTemplate,
			options:    customboot,
			wantReason: "its boot options are templated",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hw := workflowTestHardware(true)
			scope := workflowTestScope(g, tc.data, hw)
			scope.tinkerbellMachine.Spec.BootOptions = tc.options

			g.Expect(scope.createWorkflow(hw)).To(Succeed())

			template := &tinkv1.Template{}
			g.Expect(scope.tinkerbellClient.Get(scope.ctx, machineKey(), template)).To(Succeed(), "Expected the Template to be created")
			g.Expect(template.Spec.Data).To(Equal(ptr.To(tc.data)))
			expectOwnedByMachine(g, scope, template)

			g.Expect(listTasks(g, scope)).To(BeEmpty())

			wf := getWorkflow(g, scope)
			g.Expect(wf.Annotations).To(HaveKeyWithValue(conversion.AnnotationTemplate, wfMachine),
				"the Workflow runs the Template in compatibility mode")
			g.Expect(wf.Annotations).To(HaveKeyWithValue(AnnotationCompatibilityMode, ContainSubstring(tc.wantReason)))
			g.Expect(wf.Annotations).NotTo(HaveKey(conversion.AnnotationV1Alpha1))
			g.Expect(wf.Spec.Tasks).To(HaveLen(1))
			g.Expect(wf.Spec.Tasks[0].TaskRef).To(BeZero(), "a compatibility mode Workflow references no Task")
			expectOwnedByMachine(g, scope, wf)
			expectV1Alpha1View(g, scope, hw, wf)
		})
	}
}

// A machine whose Template an earlier CAPT created keeps running it.
func Test_createWorkflow_keeps_running_an_existing_template(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	existing := "version: \"0.1\"\nname: old\n"
	hw := workflowTestHardware(false)
	template := &tinkv1.Template{
		ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace},
		Spec:       tinkv1.TemplateSpec{Data: &existing},
	}
	scope := workflowTestScope(g, talosTemplate, hw, template)

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	got := &tinkv1.Template{}
	g.Expect(scope.tinkerbellClient.Get(scope.ctx, machineKey(), got)).To(Succeed())
	g.Expect(got.Spec.Data).To(Equal(&existing), "the existing Template is not rewritten")
	g.Expect(listTasks(g, scope)).To(BeEmpty())

	wf := getWorkflow(g, scope)
	g.Expect(wf.Annotations).To(HaveKeyWithValue(AnnotationCompatibilityMode, ContainSubstring("Template already exists")))
	g.Expect(wf.Spec.Tasks[0].TaskRef).To(BeZero())
}

// A Task of the name the machine's Task would get, which is not the machine's, is left alone;
// the machine runs its Template in compatibility mode.
func Test_createWorkflow_falls_back_when_a_task_name_is_taken(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	foreign := &tinkv2.Task{
		ObjectMeta: metav1.ObjectMeta{Name: wfMachine + "-talos", Namespace: wfNamespace},
		Spec:       tinkv2.TaskSpec{Actions: []tinkv2.Action{{Name: "theirs", Image: "alpine"}}},
	}
	scope := workflowTestScope(g, talosTemplate, hw, foreign)

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	got := &tinkv2.Task{}
	g.Expect(scope.tinkerbellClient.Get(scope.ctx, client.ObjectKeyFromObject(foreign), got)).To(Succeed())
	g.Expect(got.Spec).To(Equal(foreign.Spec), "a Task that is not the machine's is not CAPT's to change")

	g.Expect(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv1.Template{})).To(Succeed())

	wf := getWorkflow(g, scope)
	g.Expect(wf.Annotations).To(HaveKeyWithValue(AnnotationCompatibilityMode, ContainSubstring("does not belong to this machine")))
}

// Recreating a machine's Workflow (it was deleted, or creating it failed after the Tasks were
// created) brings the machine's Tasks in line with its template and removes stale ones.
func Test_createWorkflow_replaces_tasks_left_by_an_earlier_attempt(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	labels := map[string]string{LabelMachineName: wfMachine, LabelMachineNamespace: wfNamespace}
	stale := &tinkv2.Task{
		ObjectMeta: metav1.ObjectMeta{Name: wfMachine + "-talos", Namespace: wfNamespace, Labels: labels},
		Spec:       tinkv2.TaskSpec{Actions: []tinkv2.Action{{Name: "stale", Image: "alpine"}}},
	}
	gone := &tinkv2.Task{
		ObjectMeta: metav1.ObjectMeta{Name: wfMachine + "-old", Namespace: wfNamespace, Labels: labels},
		Spec:       tinkv2.TaskSpec{Actions: []tinkv2.Action{{Name: "old", Image: "alpine"}}},
	}
	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, talosTemplate, hw, stale, gone)

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	conv, err := conversion.ConvertTemplate(talosTemplate, templateFuncs())
	g.Expect(err).NotTo(HaveOccurred())

	want := conversion.TemplateTasks(scope.templateObject(""), conv)

	tasks := listTasks(g, scope)
	g.Expect(tasks).To(HaveLen(1), "the Task the template no longer has is removed")
	g.Expect(tasks[0].Name).To(Equal(stale.Name))
	g.Expect(tasks[0].Spec).To(Equal(want[0].Spec))
	g.Expect(getWorkflow(g, scope).Annotations).NotTo(HaveKey(AnnotationCompatibilityMode))
}

func Test_createWorkflow_requires_storage_devices(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	hw.Spec.StorageDevices = nil
	scope := workflowTestScope(g, talosTemplate, hw)

	g.Expect(scope.createWorkflow(hw)).To(MatchError(ErrHardwareMissingDiskConfiguration))
	g.Expect(listTasks(g, scope)).To(BeEmpty())
	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())
}

// CAPT follows a Workflow through status.metadata.workflow.state, the v1alpha2 form of the
// v1alpha1 status.state it read before.
func Test_reconcile_follows_the_workflow_state(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		state     tinkv2.State
		wantErr   error
		wantReady bool
	}{
		"not_started": {},
		"preparing":   {state: tinkv2.StatePreparing},
		"pending":     {state: tinkv2.StatePending},
		"running":     {state: tinkv2.StateRunning},
		"post":        {state: tinkv2.StatePost},
		"failed":      {state: tinkv2.StateFailed, wantErr: errWorkflowFailed},
		"timed_out":   {state: tinkv2.StateTimeout, wantErr: errWorkflowTimeout},
		"succeeded":   {state: tinkv2.StateSuccess, wantReady: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hw := workflowTestHardware(false)
			wf := &tinkv2.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace},
				Status:     tinkv2.WorkflowStatus{Metadata: tinkv2.WorkflowMetadata{Workflow: tinkv2.Metadata{State: tc.state}}},
			}
			scope := workflowTestScope(g, talosTemplate, hw, wf)

			stored := &tinkv2.Hardware{}
			g.Expect(scope.tinkerbellClient.Get(scope.ctx, client.ObjectKeyFromObject(hw), stored)).To(Succeed())

			err := scope.reconcile(stored)
			if tc.wantErr != nil {
				g.Expect(err).To(MatchError(tc.wantErr))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}

			g.Expect(scope.tinkerbellMachine.Status.Ready).To(Equal(tc.wantReady))

			persisted := &tinkv2.Hardware{}
			g.Expect(scope.tinkerbellClient.Get(scope.ctx, client.ObjectKeyFromObject(hw), persisted)).To(Succeed())

			if tc.wantReady {
				g.Expect(persisted.Annotations).To(HaveKeyWithValue(HardwareProvisionedAnnotation, "true"))
			} else {
				g.Expect(persisted.Annotations).NotTo(HaveKey(HardwareProvisionedAnnotation))
			}
		})
	}
}

// Deleting a machine removes everything CAPT created for it to provision: the Workflow, its
// Tasks, and the Template of a Workflow in compatibility mode or of an earlier CAPT.
func Test_removeDependencies_removes_the_workflow_tasks_and_template(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	labels := map[string]string{LabelMachineName: wfMachine, LabelMachineNamespace: wfNamespace}
	data := "version: \"0.1\"\n"
	objects := []client.Object{
		&tinkv1.Template{ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace, Labels: labels}, Spec: tinkv1.TemplateSpec{Data: &data}},
		&tinkv2.Task{ObjectMeta: metav1.ObjectMeta{Name: wfMachine + "-talos", Namespace: wfNamespace, Labels: labels}},
		&tinkv2.Task{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: wfNamespace}},
		&tinkv2.Workflow{ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace, Labels: labels}},
	}
	scope := workflowTestScope(g, talosTemplate, objects...)

	g.Expect(scope.removeDependencies()).To(Succeed())

	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv1.Template{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())

	tasks := listTasks(g, scope)
	g.Expect(tasks).To(HaveLen(1))
	g.Expect(tasks[0].Name).To(Equal("someone-elses"), "only the machine's Tasks are removed")
}

// A disk index nested in another call, as in CAPT's Quick Start template, converts: the Workflow runs
// the template's Task natively.
func Test_createWorkflow_runs_a_nested_disk_index_natively(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(true)
	scope := workflowTestScope(g, partitionTemplate, hw)
	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	tasks := listTasks(g, scope)
	g.Expect(tasks).To(HaveLen(1))
	g.Expect(tasks[0].Spec.Actions[0].Env).To(HaveKeyWithValue("DEST_DISK",
		"{{formatPartition ((index .hardware.spec.storageDevices 0).name) 3}}"))
	expectOwnedByMachine(g, scope, &tasks[0])

	wf := getWorkflow(g, scope)
	g.Expect(wf.Annotations).NotTo(HaveKey(AnnotationCompatibilityMode))
	g.Expect(wf.Spec.Tasks).To(HaveLen(1))
	g.Expect(wf.Spec.Tasks[0].TaskRef.Name).To(Equal(tasks[0].Name))
	expectV1Alpha1View(g, scope, hw, wf)
}
