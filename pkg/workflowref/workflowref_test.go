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

package workflowref_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
	"github.com/tinkerbell/cluster-api-provider-tinkerbell/pkg/workflowref"
)

// fixtures returns a Machine m1/capi of cluster c1 bootstrapped by TalosConfig m1-config, its
// TinkerbellMachine tm1/capi, and the Hardware hw1/tink it claimed.
func fixtures() (*clusterv1.Machine, *infrastructurev1.TinkerbellMachine, *tinkv2.Hardware) {
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "capi"},
		Spec: clusterv1.MachineSpec{
			ClusterName: "c1",
			Bootstrap: clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "TalosConfig", Name: "m1-config",
			}},
		},
	}
	tm := &infrastructurev1.TinkerbellMachine{ObjectMeta: metav1.ObjectMeta{Name: "tm1", Namespace: "capi"}}
	hw := &tinkv2.Hardware{ObjectMeta: metav1.ObjectMeta{Name: "hw1", Namespace: "tink"}}

	return machine, tm, hw
}

func TestVariables(t *testing.T) {
	t.Parallel()

	machine, tm, hw := fixtures()

	want := map[string]string{
		"machine_name":            "m1",
		"machine_namespace":       "capi",
		"cluster_name":            "c1",
		"bootstrap_config_name":   "m1-config",
		"tinkerbell_machine_name": "tm1",
		"hardware_name":           "hw1",
		"hardware_namespace":      "tink",
	}
	if diff := cmp.Diff(want, workflowref.Variables(machine, tm, hw)); diff != "" {
		t.Errorf("Variables (-want +got):\n%s", diff)
	}

	// Without the Machine, the values it supplies are empty, and the namespace still comes
	// from the TinkerbellMachine, which is always in the Machine's namespace.
	want["machine_name"], want["cluster_name"], want["bootstrap_config_name"] = "", "", ""
	if diff := cmp.Diff(want, workflowref.Variables(nil, tm, hw)); diff != "" {
		t.Errorf("Variables without a Machine (-want +got):\n%s", diff)
	}
}

// The admission check accepts the variables Names lists, the controller renders with
// Variables: if they ever disagreed, valid machines would be rejected or typos would pass
// admission.
func TestNamesMatchVariables(t *testing.T) {
	t.Parallel()

	machine, tm, hw := fixtures()

	variables := slices.Sorted(maps.Keys(workflowref.Variables(machine, tm, hw)))
	if diff := cmp.Diff(variables, workflowref.Names()); diff != "" {
		t.Errorf("Names differ from Variables keys (-variables +names):\n%s", diff)
	}
}

// Reference templates come from whoever can write a TinkerbellMachine, so they may only
// substitute variables: anything that could make CAPT loop, allocate or recurse is refused
// before it runs.
func TestCheck(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{
		"",
		"literal",
		"{{ .machine_name }}",
		"{{ .machine_name }}-{{ .cluster_name }}",
		"{{- .machine_name -}}",
	} {
		if err := workflowref.Check(ok); err != nil {
			t.Errorf("Check(%q) = %v; want nil", ok, err)
		}
	}

	for text, want := range map[string]string{
		"{{ .machine_namespcae }}":               `unknown variable "machine_namespcae"`,
		"{{range 30000000}}{{end}}":              "only {{ .variable }} substitutions are allowed",
		`{{printf "%01000000d" 0}}`:              "only {{ .variable }} substitutions are allowed",
		`{{define "a"}}x{{end}}{{template "a"}}`: "only {{ .variable }} substitutions are allowed",
		`{{ .machine_name | printf "%s" }}`:      "only {{ .variable }} substitutions are allowed",
		"{{ $x := .machine_name }}{{ $x }}":      "only {{ .variable }} substitutions are allowed",
		"{{ if .machine_name }}a{{ end }}":       "only {{ .variable }} substitutions are allowed",
		"{{ .machine_name.x }}":                  "only {{ .variable }} substitutions are allowed",
		"{{ len .machine_name }}":                "only {{ .variable }} substitutions are allowed",
		"{{ .machine_name":                       "unclosed action",
	} {
		err := workflowref.Check(text)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v; want an error containing %q", text, err, want)
		}

		// Execute applies the same rule, so nothing refused here ever runs.
		if _, err := workflowref.Execute(text, workflowref.Variables(fixtures())); err == nil {
			t.Errorf("Execute(%q) succeeded; want an error", text)
		}
	}
}

func TestExecute(t *testing.T) {
	t.Parallel()

	machine, tm, hw := fixtures()
	vars := workflowref.Variables(machine, tm, hw)

	got, err := workflowref.Execute("{{ .machine_name }}-talos", vars)
	if err != nil || got != "m1-talos" {
		t.Errorf(`Execute("{{ .machine_name }}-talos") = %q, %v; want "m1-talos", nil`, got, err)
	}

	got, err = workflowref.Execute("literal", vars)
	if err != nil || got != "literal" {
		t.Errorf(`Execute("literal") = %q, %v; want "literal", nil`, got, err)
	}

	if _, err := workflowref.Execute("{{ .machine_namespcae }}", vars); err == nil || !strings.Contains(err.Error(), "machine_namespcae") {
		t.Errorf("Execute with an unknown variable: err = %v; want an error naming machine_namespcae", err)
	}

	if _, err := workflowref.Execute("{{ .machine_name", vars); err == nil {
		t.Error("Execute with a parse error: err = nil; want an error")
	}
}

func TestRender(t *testing.T) {
	t.Parallel()

	talos := infrastructurev1.WorkflowReference{
		Group:     "bootstrap.cluster.x-k8s.io",
		Version:   "v1beta1",
		Resource:  "talosconfigs",
		Name:      "{{ .bootstrap_config_name }}",
		Namespace: "{{ .machine_namespace }}",
	}

	with := func(name, namespace string) infrastructurev1.WorkflowReference {
		ref := talos
		ref.Name, ref.Namespace = name, namespace

		return ref
	}

	rendered := tinkv2.Reference{
		Group:     "bootstrap.cluster.x-k8s.io",
		Version:   "v1beta1",
		Resource:  "talosconfigs",
		Name:      "m1-config",
		Namespace: "capi",
	}

	for _, tt := range []struct {
		name          string
		ref           infrastructurev1.WorkflowReference
		noBootstrap   bool
		want          tinkv2.Reference
		wantErr       error
		wantErrSubstr string
	}{
		{name: "templates rendered, group version and resource kept", ref: talos, want: rendered},
		{name: "empty namespace defaults to the machine's", ref: with("{{ .bootstrap_config_name }}", ""), want: rendered},
		{name: "literal name and namespace", ref: with("m1-config", "capi"), want: rendered},
		{name: "another namespace", ref: with("m1-config", "other"), wantErr: workflowref.ErrOutsideNamespace, wantErrSubstr: "other"},
		{name: "the Hardware's namespace", ref: with("m1-config", "{{ .hardware_namespace }}"), wantErr: workflowref.ErrOutsideNamespace, wantErrSubstr: "tink"},
		{name: "empty bootstrap config name", ref: talos, noBootstrap: true, wantErr: workflowref.ErrNotRendered, wantErrSubstr: "name"},
		{name: "name that is not a DNS-1123 subdomain", ref: with("{{ .machine_name }}_x", ""), wantErr: workflowref.ErrNotRendered, wantErrSubstr: "m1_x"},
		{name: "unknown variable", ref: with("{{ .nope }}", ""), wantErr: workflowref.ErrNotRendered, wantErrSubstr: "nope"},
		{name: "namespace parse error", ref: with("m1-config", "{{ .machine_namespace"), wantErr: workflowref.ErrNotRendered, wantErrSubstr: "namespace"},
		{name: "range in name", ref: with("{{range 30000000}}{{end}}", ""), wantErr: workflowref.ErrNotRendered, wantErrSubstr: "only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			machine, tm, hw := fixtures()
			if tt.noBootstrap {
				machine.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{}
			}

			got, err := workflowref.Render(tt.ref, workflowref.Variables(machine, tm, hw))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("Render error = %v; want %v containing %q", err, tt.wantErr, tt.wantErrSubstr)
				}

				return
			}

			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Render (-want +got):\n%s", diff)
			}
		})
	}
}
