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

// Package workflowref renders the references a TinkerbellMachine adds to its Workflow: the
// machine variables their name and namespace templates may use, and the rendering itself.
// The admission webhooks and the machine controller both use it, so they always agree on the
// variables.
package workflowref

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/template"
	tparse "text/template/parse"

	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// The machine variables a reference's name and namespace templates may use.
const (
	// MachineName is the Cluster API Machine's name.
	MachineName = "machine_name"
	// MachineNamespace is the Machine's namespace, which is also the TinkerbellMachine's.
	MachineNamespace = "machine_namespace"
	// ClusterName is the Machine's spec.clusterName.
	ClusterName = "cluster_name"
	// BootstrapConfigName is the name of the Machine's bootstrap config
	// (spec.bootstrap.configRef.name), or "" when it has none.
	BootstrapConfigName = "bootstrap_config_name"
	// TinkerbellMachineName is the TinkerbellMachine's name.
	TinkerbellMachineName = "tinkerbell_machine_name"
	// HardwareName is the name of the Hardware the machine claimed.
	HardwareName = "hardware_name"
	// HardwareNamespace is the Hardware's namespace, where the Workflow is created.
	HardwareNamespace = "hardware_namespace"
)

var (
	// ErrNotRendered is returned when a reference's name or namespace cannot be rendered, or
	// renders to a name that cannot name an object.
	ErrNotRendered = errors.New("reference not rendered")

	// ErrOutsideNamespace is returned when a reference renders into a namespace other than the
	// machine's.
	ErrOutsideNamespace = errors.New("reference outside the machine's namespace")
)

// Names returns the machine variable names, sorted.
func Names() []string {
	return []string{
		BootstrapConfigName,
		ClusterName,
		HardwareName,
		HardwareNamespace,
		MachineName,
		MachineNamespace,
		TinkerbellMachineName,
	}
}

// Variables returns the machine variables of the TinkerbellMachine tm, owned by machine, that
// claimed hw. machine_namespace comes from tm, which is always in the Machine's namespace, so
// the namespace a reference is limited to never depends on machine. The values machine
// supplies are "" when it is nil.
func Variables(machine *clusterv1.Machine, tm *infrastructurev1.TinkerbellMachine, hw *tinkv2.Hardware) map[string]string {
	vars := map[string]string{
		MachineNamespace:      tm.Namespace,
		TinkerbellMachineName: tm.Name,
		HardwareName:          hw.Name,
		HardwareNamespace:     hw.Namespace,
		MachineName:           "",
		ClusterName:           "",
		BootstrapConfigName:   "",
	}

	if machine != nil {
		vars[MachineName] = machine.Name
		vars[ClusterName] = machine.Spec.ClusterName
		vars[BootstrapConfigName] = machine.Spec.Bootstrap.ConfigRef.Name
	}

	return vars
}

// errNotSubstitution is returned for a template that does anything but substitute variables.
var errNotSubstitution = errors.New("only {{ .variable }} substitutions are allowed")

// parse parses text as a reference template, refusing anything but literal text and
// {{ .variable }} substitutions of the machine variables. Reference templates are written by
// whoever can write a TinkerbellMachine, so functions, pipelines, control structures and
// nested templates, which could make CAPT loop, allocate or recurse without bound, never run.
func parse(text string) (*template.Template, error) {
	tmpl, err := template.New("reference").Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, err //nolint:wrapcheck // the template error says what is wrong
	}

	if len(tmpl.Templates()) > 1 {
		return nil, errNotSubstitution
	}

	if tmpl.Tree == nil || tmpl.Root == nil {
		return tmpl, nil
	}

	known := Names()

	for _, node := range tmpl.Root.Nodes {
		if err := checkNode(node, known); err != nil {
			return nil, err
		}
	}

	return tmpl, nil
}

// checkNode accepts literal text and a {{ .variable }} substitution of one of known.
func checkNode(node tparse.Node, known []string) error {
	switch node := node.(type) {
	case *tparse.TextNode:
		return nil
	case *tparse.ActionNode:
		name, ok := substitution(node)
		if !ok {
			return errNotSubstitution
		}

		if !slices.Contains(known, name) {
			return fmt.Errorf("unknown variable %q", name)
		}

		return nil
	default:
		return errNotSubstitution
	}
}

// substitution returns the variable an action substitutes, when it is exactly {{ .variable }}.
func substitution(action *tparse.ActionNode) (string, bool) {
	pipe := action.Pipe
	if pipe == nil || len(pipe.Decl) > 0 || pipe.IsAssign || len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return "", false
	}

	field, ok := pipe.Cmds[0].Args[0].(*tparse.FieldNode)
	if !ok || len(field.Ident) != 1 {
		return "", false
	}

	return field.Ident[0], true
}

// Check reports whether text is a valid reference template: literal text and {{ .variable }}
// substitutions of the machine variables only. It runs nothing.
func Check(text string) error {
	_, err := parse(text)

	return err
}

// Execute renders text as a reference template with vars. A template Check refuses never runs.
func Execute(text string, vars map[string]string) (string, error) {
	tmpl, err := parse(text)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, vars); err != nil {
		return "", err //nolint:wrapcheck // the template error says what is wrong
	}

	return out.String(), nil
}

// Render returns ref as a Tinkerbell Reference, with its name and namespace rendered with
// vars and its group, version and resource as written. An empty namespace is the machine's
// (vars[MachineNamespace]); any other namespace is refused with ErrOutsideNamespace. A name or
// namespace that does not render, or a name that is not a DNS-1123 subdomain, is refused with
// ErrNotRendered.
func Render(ref infrastructurev1.WorkflowReference, vars map[string]string) (tinkv2.Reference, error) {
	name, err := Execute(ref.Name, vars)
	if err != nil {
		return tinkv2.Reference{}, fmt.Errorf("%w: name: %w", ErrNotRendered, err)
	}

	if name == "" {
		return tinkv2.Reference{}, fmt.Errorf("%w: name %q rendered empty", ErrNotRendered, ref.Name)
	}

	if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return tinkv2.Reference{}, fmt.Errorf("%w: name %q: %s", ErrNotRendered, name, strings.Join(problems, "; "))
	}

	namespace, err := Execute(ref.Namespace, vars)
	if err != nil {
		return tinkv2.Reference{}, fmt.Errorf("%w: namespace: %w", ErrNotRendered, err)
	}

	machineNamespace := vars[MachineNamespace]
	if namespace == "" {
		namespace = machineNamespace
	}

	if namespace != machineNamespace {
		return tinkv2.Reference{}, fmt.Errorf("%w: namespace %q is not the machine's namespace %q", ErrOutsideNamespace, namespace, machineNamespace)
	}

	return tinkv2.Reference{
		Group:     ref.Group,
		Version:   ref.Version,
		Resource:  ref.Resource,
		Name:      name,
		Namespace: namespace,
	}, nil
}
