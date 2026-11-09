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
	"strings"

	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
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
)

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
