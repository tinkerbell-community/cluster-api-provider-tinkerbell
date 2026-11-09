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

package webhooks_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

const (
	sourceRuleMessage = "at most one of tasks, templateInline and templateRef may be set"
	entryRuleMessage  = "exactly one of taskRef and taskInline must be set"
)

func inlineEntry(name string) v1beta2.MachineTask {
	return v1beta2.MachineTask{
		Name:       name,
		TaskInline: &tinkv2.TaskSpec{Actions: []tinkv2.Action{{Name: "a", Image: "alpine"}}},
	}
}

func refEntry(name, task string) v1beta2.MachineTask {
	return v1beta2.MachineTask{Name: name, TaskRef: &v1beta2.TaskRef{Name: task}}
}

func machineWith(config v1beta2.TinkerbellMachineConfig) *v1beta2.TinkerbellMachine {
	return &v1beta2.TinkerbellMachine{Spec: v1beta2.TinkerbellMachineSpec{TinkerbellMachineConfig: config}}
}

// TestCRDValidation runs CAPT's CRDs in a real API server, so their CEL rules and list
// markers (which webhooks and the fake client do not apply) are exercised. It needs envtest's
// binaries (KUBEBUILDER_ASSETS) and is skipped without them.
func TestCRDValidation(t *testing.T) {
	t.Parallel()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run against CAPT's CRDs")
	}

	g := NewWithT(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())

	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	g.Expect(v1beta2.AddToScheme(scheme)).To(Succeed())

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	rejected := rejectedObjects()

	for i, tt := range rejected {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			tt.obj.SetName(fmt.Sprintf("rejected-%d", i))
			tt.obj.SetNamespace(metav1.NamespaceDefault)

			err := c.Create(context.Background(), tt.obj)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tt.want))
		})
	}

	accepted := acceptedObjects()

	for i, tt := range accepted {
		t.Run("accepts "+tt.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			tt.obj.SetName(fmt.Sprintf("accepted-%d", i))
			tt.obj.SetNamespace(metav1.NamespaceDefault)

			g.Expect(c.Create(context.Background(), tt.obj)).To(Succeed())
		})
	}
}

// rejectedObject is an object the CRDs must refuse, with part of the reason.
type rejectedObject struct {
	name string
	obj  client.Object
	want string
}

func rejectedObjects() []rejectedObject {
	bootISO := inlineEntry("a")
	bootISO.BootOptions = &tinkv2.BootOptions{BootMode: "iso"}

	both := inlineEntry("a")
	both.TaskRef = &v1beta2.TaskRef{Name: "shared"}

	withRefs := inlineEntry("a")
	withRefs.TaskInline.References = map[string]tinkv2.Reference{"secret": {Name: "s", Resource: "secrets", Version: "v1"}}

	return []rejectedObject{
		{
			name: "machine with tasks and templateInline",
			obj:  machineWith(v1beta2.TinkerbellMachineConfig{Tasks: []v1beta2.MachineTask{inlineEntry("a")}, TemplateInline: "t"}),
			want: sourceRuleMessage,
		},
		{
			name: "cluster with tasks and templateRef",
			obj: &v1beta2.TinkerbellCluster{Spec: v1beta2.TinkerbellClusterSpec{
				Tasks:       []v1beta2.MachineTask{inlineEntry("a")},
				TemplateRef: &v1beta2.ObjectRef{Name: "t"},
			}},
			want: sourceRuleMessage,
		},
		{
			name: "entry with taskRef and taskInline",
			obj:  machineWith(v1beta2.TinkerbellMachineConfig{Tasks: []v1beta2.MachineTask{both}}),
			want: entryRuleMessage,
		},
		{
			name: "entry with neither taskRef nor taskInline",
			obj:  machineWith(v1beta2.TinkerbellMachineConfig{Tasks: []v1beta2.MachineTask{{Name: "a"}}}),
			want: entryRuleMessage,
		},
		{
			name: "two entries with the same name",
			obj:  machineWith(v1beta2.TinkerbellMachineConfig{Tasks: []v1beta2.MachineTask{inlineEntry("a"), refEntry("a", "shared")}}),
			want: "Duplicate value",
		},
		{
			name: "inline task with references",
			obj:  machineWith(v1beta2.TinkerbellMachineConfig{Tasks: []v1beta2.MachineTask{withRefs}}),
			want: "taskInline cannot set references",
		},
		{
			name: "entry with the legacy iso boot mode",
			obj:  machineWith(v1beta2.TinkerbellMachineConfig{Tasks: []v1beta2.MachineTask{bootISO}}),
			want: "Unsupported value",
		},
	}
}

// acceptedObject is an object the CRDs must take.
type acceptedObject struct {
	name string
	obj  client.Object
}

func acceptedObjects() []acceptedObject {
	mixed := []v1beta2.MachineTask{inlineEntry("install"), refEntry("cleanup", "shared")}

	return []acceptedObject{
		{name: "machine with a mixed list", obj: machineWith(v1beta2.TinkerbellMachineConfig{Tasks: mixed})},
		{
			name: "machine template with a mixed list",
			obj: &v1beta2.TinkerbellMachineTemplate{Spec: v1beta2.TinkerbellMachineTemplateSpec{
				Template: v1beta2.TinkerbellMachineTemplateResource{Spec: v1beta2.TinkerbellMachineConfig{Tasks: mixed}},
			}},
		},
		{name: "cluster with a mixed list", obj: &v1beta2.TinkerbellCluster{Spec: v1beta2.TinkerbellClusterSpec{Tasks: mixed}}},
		{name: "machine with templateInline only", obj: machineWith(v1beta2.TinkerbellMachineConfig{TemplateInline: "t"})},
	}
}
