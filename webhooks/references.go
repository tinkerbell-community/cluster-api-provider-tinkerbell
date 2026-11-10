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
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
	"github.com/tinkerbell/cluster-api-provider-tinkerbell/pkg/workflowref"
)

// referencesRuleMessage is the message of the TinkerbellMachineConfig CEL rule that keeps
// references and vars away from the deprecated template fields.
const referencesRuleMessage = "references and vars apply to tasks and cannot be set with templateInline or templateRef"

// validateReferences checks the references and vars of the machine config at path, for a
// machine in namespace:
//   - group, version and resource are not templates;
//   - name and namespace are literal text and {{ .variable }} substitutions of known machine
//     variables, checked without running them, so a misspelt variable is caught before any
//     machine exists and nothing a machine writer submits runs at admission;
//   - a literal namespace is empty or the machine's (a templated one is checked when the
//     Workflow is created);
//   - neither field is set next to templateInline or templateRef.
//
// The CRD's CEL rule says the last; the webhook also sees a v1beta1 write whose conversion
// restores stashed references next to a template.
func validateReferences(path *field.Path, namespace string, config *infrastructurev1.TinkerbellMachineConfig) field.ErrorList {
	var errs field.ErrorList

	for _, key := range slices.Sorted(maps.Keys(config.References)) {
		errs = append(errs, validateReference(path.Child("references").Key(key), namespace, config.References[key])...)
	}

	if config.TemplateInline != "" || config.TemplateRef != nil {
		switch {
		case len(config.References) > 0:
			errs = append(errs, field.Forbidden(path.Child("references"), referencesRuleMessage))
		case len(config.Vars) > 0:
			errs = append(errs, field.Forbidden(path.Child("vars"), referencesRuleMessage))
		}
	}

	return errs
}

// validateReference checks one reference at path, for a machine in namespace.
func validateReference(path *field.Path, namespace string, ref infrastructurev1.WorkflowReference) field.ErrorList {
	var errs field.ErrorList

	for _, literal := range []struct {
		name, value string
	}{{"group", ref.Group}, {"version", ref.Version}, {"resource", ref.Resource}} {
		if strings.Contains(literal.value, "{{") {
			errs = append(errs, field.Invalid(path.Child(literal.name), literal.value,
				"is not a template: only name and namespace are rendered"))
		}
	}

	known := strings.Join(workflowref.Names(), ", ")

	for _, templated := range []struct {
		name, value string
	}{{"name", ref.Name}, {"namespace", ref.Namespace}} {
		if err := workflowref.Check(templated.value); err != nil {
			errs = append(errs, field.Invalid(path.Child(templated.name), templated.value,
				fmt.Sprintf("%v; known variables: %s", err, known)))
		}
	}

	if ref.Namespace != "" && !strings.Contains(ref.Namespace, "{{") && ref.Namespace != namespace {
		errs = append(errs, field.Forbidden(path.Child("namespace"),
			fmt.Sprintf("must be empty or the machine's namespace %q", namespace)))
	}

	return errs
}
