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
	"errors"
	"strings"

	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/cluster-api/util/record"

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
			record.Warnf(scope.tinkerbellMachine, conflictingHardwareAnnotationsReason,
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
			record.Warnf(scope.tinkerbellMachine, bootOptionsIgnoredReason,
				"spec.bootOptions is ignored: the tasks entries set their own bootOptions")
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

// defaultBootOptions returns, as v1alpha2, the boot options CAPT gives a machine's Workflow
// from spec.bootOptions: netboot toggled, plus the boot mode when the Hardware has a BMC.
// They are those of the v1alpha1 Workflow CAPT has always built (newWorkflow), converted.
func (scope *machineReconcileScope) defaultBootOptions(hw *tinkv2.Hardware) (*tinkv2.BootOptions, error) {
	wf, err := scope.newWorkflow(hw)
	if err != nil {
		return nil, err
	}

	if templatedBootOptions(wf) {
		record.Warnf(scope.tinkerbellMachine, templatedCustombootConfigReason, "%s", ErrTemplatedCustombootConfig.Error())

		return nil, ErrTemplatedCustombootConfig
	}

	if len(wf.Spec.Tasks) == 0 || wf.Spec.Tasks[0].Hardware == nil {
		return nil, nil
	}

	return wf.Spec.Tasks[0].Hardware.BootOptions, nil
}
