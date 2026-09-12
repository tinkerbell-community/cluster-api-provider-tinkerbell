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
	"testing"

	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// Test_workflowTemplateData asserts the Workflow's hardwareMap carries the hardware
// identifier and nothing else. Which operating system image a machine gets is not the
// infrastructure provider's concern: a template reads that from the Hardware object itself.
func Test_workflowTemplateData(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := &tinkv1.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "myHardware", Namespace: "myNamespace"},
		Spec: tinkv1.HardwareSpec{
			Metadata: &tinkv1.HardwareMetadata{
				Instance: &tinkv1.MetadataInstance{ID: "de:ad:be:ef:00:01"},
			},
		},
	}

	scope := &machineReconcileScope{
		tinkerbellMachine: &infrastructurev1.TinkerbellMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "myMachine", Namespace: "myNamespace"},
		},
	}

	g.Expect(scope.workflowTemplateData(hw)).To(Equal(map[string]string{"device_1": "de:ad:be:ef:00:01"}))
}
