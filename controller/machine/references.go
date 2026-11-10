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
	"fmt"
	"maps"
	"slices"
	"strings"

	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"

	"github.com/tinkerbell/cluster-api-provider-tinkerbell/pkg/workflowref"
)

// Reasons of the events recorded about a machine's Workflow references.
const (
	referenceNotRenderedReason      = "ReferenceNotRendered"
	referenceOutsideNamespaceReason = "ReferenceOutsideNamespace"
	referencesUnsupportedReason     = "ReferencesUnsupported"
	referencesIgnoredReason         = "ReferencesIgnored"
)

var (
	// ErrReferenceNotRendered is returned when a machine's references cannot be rendered into
	// references its Workflow may carry.
	ErrReferenceNotRendered = errors.New("workflow references could not be rendered")

	// ErrReferencesUnsupported is returned for a machine with references whose Tinkerbell is
	// external: it would look the objects up in its own cluster.
	ErrReferencesUnsupported = errors.New("workflow references are not supported with an external Tinkerbell")
)

// machineVariables returns the machine variables of the machine, which claimed hw.
func (scope *machineReconcileScope) machineVariables(hw *tinkv2.Hardware) map[string]string {
	return workflowref.Variables(scope.machine, scope.tinkerbellMachine, hw)
}

// workflowReferences returns the machine's references rendered for its Workflow on hw, or nil
// when it has none. Each reference that cannot be rendered, or would leave the machine's
// namespace, is recorded on the TinkerbellMachine, and then ErrReferenceNotRendered names them
// all. With an external Tinkerbell, references are refused with ErrReferencesUnsupported.
func (scope *machineReconcileScope) workflowReferences(hw *tinkv2.Hardware) (map[string]tinkv2.Reference, error) {
	refs := scope.tinkerbellMachine.Spec.References
	if len(refs) == 0 {
		return nil, nil
	}

	if scope.isExternal() {
		scope.warnf(referencesUnsupportedReason,
			"spec.references cannot be used with an external Tinkerbell, which would look the objects up in its own cluster")

		return nil, ErrReferencesUnsupported
	}

	vars := scope.machineVariables(hw)
	rendered := make(map[string]tinkv2.Reference, len(refs))

	var failed []string

	for _, key := range slices.Sorted(maps.Keys(refs)) {
		ref, err := workflowref.Render(refs[key], vars)
		if err != nil {
			reason := referenceNotRenderedReason
			if errors.Is(err, workflowref.ErrOutsideNamespace) {
				reason = referenceOutsideNamespaceReason
			}

			scope.warnf(reason, "spec.references[%s]: %v", key, err)
			failed = append(failed, key)

			continue
		}

		rendered[key] = ref
	}

	if len(failed) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrReferenceNotRendered, strings.Join(failed, ", "))
	}

	return rendered, nil
}

// taskTemplateData is the template map of a Workflow running Tasks: the hardware identifier
// templates have always had, and the machine variables.
func (scope *machineReconcileScope) taskTemplateData(hw *tinkv2.Hardware) map[string]string {
	data := scope.workflowTemplateData(hw)
	maps.Copy(data, scope.machineVariables(hw))

	return data
}

// warnIgnoredReferences records that the machine's references and vars do nothing, because it
// is provisioned from a template.
func (scope *machineReconcileScope) warnIgnoredReferences() {
	spec := scope.tinkerbellMachine.Spec
	if len(spec.References) == 0 && len(spec.Vars) == 0 {
		return
	}

	scope.warnf(referencesIgnoredReason,
		"spec.references and spec.vars apply only to tasks; this machine is provisioned from a template")
}
