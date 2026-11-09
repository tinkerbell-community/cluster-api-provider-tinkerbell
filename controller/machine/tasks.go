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

package machine

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// Where a machine's Task list came from, for events and logs.
const (
	sourceMachine  = "TinkerbellMachine"
	sourceHardware = "Hardware"
	sourceCluster  = "TinkerbellCluster"
)

// Reasons of the events recorded about a machine's Task list.
const (
	conflictingHardwareAnnotationsReason = "ConflictingHardwareAnnotations"
	deprecatedTemplateSourceReason       = "DeprecatedTemplateSource"
	bootOptionsIgnoredReason             = "BootOptionsIgnored"
	templatedCustombootConfigReason      = "TemplatedCustombootConfig"
	taskNotFoundReason                   = "TaskNotFound"
	taskNotReadableReason                = "TaskNotReadable"
	taskManagedByCAPTReason              = "TaskManagedByCAPT"
	taskConflictReason                   = "TaskConflict"
	workflowNotRenderedReason            = "WorkflowNotRendered"
	bootModeWithoutBMCReason             = "BootModeWithoutBMC"
)

var (
	// ErrTaskNotFound is returned when a Task a machine's tasks reference does not exist.
	ErrTaskNotFound = errors.New("task not found")

	// ErrTaskConflict is returned when a Task CAPT would create for a machine's inline tasks
	// entry already exists and is not the machine's.
	ErrTaskConflict = errors.New("task belongs to something else")

	// ErrTaskManagedByCAPT is returned when a taskRef names a Task CAPT created for a machine,
	// which CAPT deletes with that machine.
	ErrTaskManagedByCAPT = errors.New("task is managed by CAPT for a machine")

	// errWorkflowNotRendered is returned while Tinkerbell cannot render the machine's Workflow.
	errWorkflowNotRendered = errors.New("workflow tasks did not render")
)

// ErrTemplatedCustombootConfig is returned when a machine running Tasks would take its boot
// options from a custombootConfig with template expressions: they are v1alpha1 data, which a
// Workflow running Tasks would render with v1alpha2 data.
var ErrTemplatedCustombootConfig = errors.New("bootOptions.custombootConfig contains template expressions, " +
	"which a Workflow running Tasks renders with v1alpha2 data; set bootOptions on the tasks entries instead")

// hardwareTaskRefs returns the Tasks hw's HardwareTaskRefsAnnotation names, in order, as
// entries referencing them in hw's namespace. Blank items are skipped and repeats kept.
func hardwareTaskRefs(hw *tinkv2.Hardware) []infrastructurev1.MachineTask {
	var tasks []infrastructurev1.MachineTask

	for item := range strings.SplitSeq(hw.Annotations[HardwareTaskRefsAnnotation], ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}

		tasks = append(tasks, infrastructurev1.MachineTask{
			Name:    name,
			TaskRef: &infrastructurev1.TaskRef{Name: name, Namespace: hw.Namespace},
		})
	}

	return tasks
}

// taskSource returns the Task list the machine is provisioned from and where it came from: the
// machine, its Hardware's HardwareTaskRefsAnnotation, or the cluster, the most specific level
// with any source winning. It returns nil when that source is a template, or there is none, so
// the template path applies exactly as before.
func (scope *machineReconcileScope) taskSource(hw *tinkv2.Hardware) ([]infrastructurev1.MachineTask, string) {
	machine := scope.tinkerbellMachine.Spec

	switch {
	case len(machine.Tasks) > 0:
		return machine.Tasks, sourceMachine
	case machine.TemplateInline != "" || machine.TemplateRef != nil:
		return nil, ""
	}

	if tasks := hardwareTaskRefs(hw); len(tasks) > 0 {
		if _, ok := hw.Annotations[HardwareTemplateOverrideAnnotation]; ok {
			scope.warnf(conflictingHardwareAnnotationsReason,
				"Hardware %s/%s sets both %s and %s; running the Tasks", hw.Namespace, hw.Name,
				HardwareTaskRefsAnnotation, HardwareTemplateOverrideAnnotation)
		}

		return tasks, sourceHardware
	}

	if _, ok := hw.Annotations[HardwareTemplateOverrideAnnotation]; ok {
		return nil, ""
	}

	if cluster := scope.tinkerbellCluster.Spec; len(cluster.Tasks) > 0 {
		return cluster.Tasks, sourceCluster
	}

	return nil, ""
}

// taskBootOptions returns the boot options of each of tasks, index-aligned with them.
//
// When any entry sets bootOptions, every entry's are copied as written, and entries without get
// none: what the user writes is what runs, as in a Tinkerbell v1alpha2 Workflow. Otherwise the
// first entry gets the boot options CAPT has always given a machine (defaultBootOptions).
func (scope *machineReconcileScope) taskBootOptions(hw *tinkv2.Hardware, tasks []infrastructurev1.MachineTask) ([]*tinkv2.BootOptions, error) {
	options := make([]*tinkv2.BootOptions, len(tasks))

	explicit := false

	for i, task := range tasks {
		if task.BootOptions != nil {
			options[i] = task.BootOptions.DeepCopy()
			explicit = true
		}
	}

	if explicit {
		if !equality.Semantic.DeepEqual(scope.tinkerbellMachine.Spec.BootOptions, infrastructurev1.BootOptions{}) {
			scope.warnf(bootOptionsIgnoredReason,
				"spec.bootOptions is ignored: the tasks entries set their own bootOptions")
		}

		for _, name := range bootModesWithoutBMC(hw, tasks) {
			scope.warnf(bootModeWithoutBMCReason,
				"tasks entry %q sets bootOptions.bootMode, but Hardware %s/%s has no BMC: Tinkerbell will fail the Workflow", name, hw.Namespace, hw.Name)
		}

		return options, nil
	}

	if len(tasks) == 0 {
		return options, nil
	}

	first, err := scope.defaultBootOptions(hw)
	if err != nil {
		return nil, err
	}

	options[0] = first

	return options, nil
}

// bootModesWithoutBMC returns the names of the entries of tasks that set a boot mode, which
// Tinkerbell carries out through the Hardware's BMC, when hw has no BMC.
func bootModesWithoutBMC(hw *tinkv2.Hardware, tasks []infrastructurev1.MachineTask) []string {
	if hasBMC(hw) {
		return nil
	}

	var names []string

	for _, task := range tasks {
		if task.BootOptions != nil && task.BootOptions.BootMode != "" {
			names = append(names, task.Name)
		}
	}

	return names
}

// defaultBootOptions returns, as v1alpha2, the boot options CAPT gives a machine's Workflow
// from spec.bootOptions: netboot toggled, plus the boot mode when the Hardware has a BMC.
// They are those of the v1alpha1 Workflow CAPT has always built (newWorkflow), converted.
func (scope *machineReconcileScope) defaultBootOptions(hw *tinkv2.Hardware) (*tinkv2.BootOptions, error) {
	wf, err := scope.newWorkflow(hw)
	if err != nil {
		return nil, err
	}

	if templatedBootOptions(wf) {
		scope.warnf(templatedCustombootConfigReason, "%s", ErrTemplatedCustombootConfig.Error())

		return nil, ErrTemplatedCustombootConfig
	}

	if len(wf.Spec.Tasks) == 0 || wf.Spec.Tasks[0].Hardware == nil {
		return nil, nil
	}

	return wf.Spec.Tasks[0].Hardware.BootOptions, nil
}

// createTaskWorkflow creates the machine's inline Tasks and a native v1alpha2 Workflow that runs
// tasks, in order, on hw. origin says where tasks came from (see taskSource).
//
// Nothing is created until everything the Workflow needs is known to be there: the referenced
// Tasks exist, no inline Task's name is taken, and the boot options resolve.
func (scope *machineReconcileScope) createTaskWorkflow(hw *tinkv2.Hardware, tasks []infrastructurev1.MachineTask, origin string) error {
	if len(hw.Spec.StorageDevices) < 1 {
		return ErrHardwareMissingDiskConfiguration
	}

	refs, inline, err := scope.resolveTasks(tasks, origin)
	if err != nil {
		return err
	}

	boot, err := scope.taskBootOptions(hw, tasks)
	if err != nil {
		return err
	}

	if err := scope.ensureTasks(inline); err != nil {
		return err
	}

	wf := scope.taskWorkflow(hw, refs, boot)
	if err := scope.setResourceOwnership(wf); err != nil {
		return fmt.Errorf("setting workflow ownership: %w", err)
	}

	if err := scope.tinkerbellClient.Create(scope.ctx, wf); err != nil {
		return fmt.Errorf("creating workflow: %w", err)
	}

	scope.log.Info("created Workflow running v1alpha2 Tasks", "workflow", wf.Name, "tasks", len(tasks), "from", origin)

	return nil
}

// resolveTasks returns the Task each of tasks runs, index-aligned with them, and the inline
// Tasks to create for the machine. Every referenced Task must exist (all missing ones are
// reported together) and be one the user manages, and an inline Task's name must not be taken by
// a Task that is not the machine's.
func (scope *machineReconcileScope) resolveTasks(tasks []infrastructurev1.MachineTask, origin string) ([]tinkv2.SimpleReference, []tinkv2.Task, error) {
	refs := make([]tinkv2.SimpleReference, len(tasks))

	var (
		inline  []tinkv2.Task
		missing []string
	)

	for i, task := range tasks {
		if task.TaskRef == nil {
			created := scope.inlineTask(task)
			if err := scope.setResourceOwnership(&created); err != nil {
				return nil, nil, fmt.Errorf("setting Task ownership: %w", err)
			}

			inline = append(inline, created)
			refs[i] = tinkv2.SimpleReference{Name: created.Name, Namespace: created.Namespace}

			continue
		}

		ref, err := scope.referencedTask(task, origin)

		switch {
		case errors.Is(err, ErrTaskNotFound):
			missing = append(missing, ref.Namespace+"/"+ref.Name)

			continue
		case err != nil:
			return nil, nil, err
		}

		refs[i] = ref
	}

	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrTaskNotFound, strings.Join(missing, ", "))
	}

	reason, err := scope.taskConflict(inline)
	if err != nil {
		return nil, nil, err
	}

	if reason != "" {
		scope.warnf(taskConflictReason, "%s", reason)

		return nil, nil, fmt.Errorf("%w: %s", ErrTaskConflict, reason)
	}

	return refs, inline, nil
}

// referencedTask returns the reference to the existing Task task names, its namespace
// defaulted to the Tinkerbell namespace. It returns ErrTaskNotFound when the Task does not exist
// and ErrTaskManagedByCAPT when CAPT created it for a machine, which would delete it with that
// machine; each problem is recorded on the TinkerbellMachine.
func (scope *machineReconcileScope) referencedTask(task infrastructurev1.MachineTask, origin string) (tinkv2.SimpleReference, error) {
	key := types.NamespacedName{Name: task.TaskRef.Name, Namespace: cmp.Or(task.TaskRef.Namespace, scope.tinkerbellNamespace())}
	ref := tinkv2.SimpleReference{Name: key.Name, Namespace: key.Namespace}

	existing := &tinkv2.Task{}
	if err := scope.tinkerbellClient.Get(scope.ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			scope.warnf(taskNotFoundReason, "Task %s, entry %q of the %s tasks, does not exist", key, task.Name, origin)

			return ref, ErrTaskNotFound
		}

		scope.warnf(taskNotReadableReason, "Task %s, entry %q of the %s tasks, cannot be read: %v", key, task.Name, origin, err)

		return ref, fmt.Errorf("getting Task %s: %w", key, err)
	}

	if owner, managed := existing.Labels[LabelMachineName]; managed {
		scope.warnf(taskManagedByCAPTReason,
			"Task %s, entry %q of the %s tasks, was created by CAPT for machine %s and is deleted with it; use taskInline, or reference a Task you manage",
			key, task.Name, origin, owner)

		return ref, fmt.Errorf("%w: %s", ErrTaskManagedByCAPT, key)
	}

	return ref, nil
}

// taskWorkflow returns the machine's Workflow running the Tasks refs on hw, each with its boot
// options from boot (index-aligned).
func (scope *machineReconcileScope) taskWorkflow(hw *tinkv2.Hardware, refs []tinkv2.SimpleReference, boot []*tinkv2.BootOptions) *tinkv2.Workflow {
	wf := &tinkv2.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: scope.tinkerbellMachine.Name, Namespace: scope.tinkerbellNamespace()},
		Spec: tinkv2.WorkflowSpec{
			Globals: &tinkv2.Extra{TemplateMap: scope.workflowTemplateData(hw)},
			Tasks:   make([]tinkv2.WorkflowTask, len(refs)),
		},
	}

	for i, ref := range refs {
		wf.Spec.Tasks[i] = tinkv2.WorkflowTask{
			AgentID: hardwareLookupID(hw),
			TaskRef: ref,
			Hardware: &tinkv2.WorkflowHardware{
				HardwareRef: &tinkv2.SimpleReference{Name: hw.Name, Namespace: hw.Namespace},
				BootOptions: boot[i],
			},
		}
	}

	return wf
}

// inlineTask returns the Task CAPT creates for the machine from an inline tasks entry.
func (scope *machineReconcileScope) inlineTask(task infrastructurev1.MachineTask) tinkv2.Task {
	return tinkv2.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scope.tinkerbellMachine.Name + "-" + task.Name,
			Namespace: scope.tinkerbellNamespace(),
		},
		Spec: *task.TaskInline.DeepCopy(),
	}
}

// notRenderedMessage returns why Tinkerbell could not render wf, while it has not started and
// says so, or "".
func notRenderedMessage(wf *tinkv2.Workflow) string {
	if wf.Status.Metadata.Workflow.State != 0 {
		return ""
	}

	for _, c := range wf.Status.Conditions {
		if c.Type == tinkv2.TemplateRenderedSuccess && c.Status == metav1.ConditionFalse {
			return cmp.Or(c.Message, c.Reason, "no reason given")
		}
	}

	return ""
}
