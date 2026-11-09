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

package webhooks

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// validateSource checks what a machine or cluster spec at path provisions from: at most one of
// tasks, templateInline and templateRef, and tasks whose entries each set exactly one of taskRef
// and taskInline under a unique name. The CRD's CEL rules say the same; the webhook also sees a
// v1beta1 write whose conversion restores stashed tasks next to a template field.
func validateSource(path *field.Path, tasks []infrastructurev1.MachineTask, templateInline string, templateRef *infrastructurev1.ObjectRef) field.ErrorList {
	var errs field.ErrorList

	sources := 0

	for _, set := range []bool{len(tasks) > 0, templateInline != "", templateRef != nil} {
		if set {
			sources++
		}
	}

	if sources > 1 {
		errs = append(errs, field.Forbidden(path.Child("tasks"), "at most one of tasks, templateInline and templateRef may be set"))
	}

	seen := make(map[string]bool, len(tasks))

	for i, task := range tasks {
		entry := path.Child("tasks").Index(i)

		if (task.TaskRef != nil) == (task.TaskInline != nil) {
			errs = append(errs, field.Invalid(entry, task.Name, "exactly one of taskRef and taskInline must be set"))
		}

		if seen[task.Name] {
			errs = append(errs, field.Duplicate(entry.Child("name"), task.Name))
		}

		seen[task.Name] = true
	}

	return errs
}

// deprecationWarnings warns about the deprecated template fields set in the spec at path.
func deprecationWarnings(path *field.Path, templateInline string, templateRef *infrastructurev1.ObjectRef) admission.Warnings {
	var warnings admission.Warnings

	replacement := path.Child("tasks").String()

	if templateInline != "" {
		warnings = append(warnings, fmt.Sprintf("%s is deprecated; use %s", path.Child("templateInline"), replacement))
	}

	if templateRef != nil {
		warnings = append(warnings, fmt.Sprintf("%s is deprecated; use %s", path.Child("templateRef"), replacement))
	}

	return warnings
}
