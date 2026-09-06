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

package webhooks_test

import (
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
	"github.com/tinkerbell/cluster-api-provider-tinkerbell/webhooks"
)

func Test_tinkerbell_machine_template_immutability(t *testing.T) {
	t.Parallel()

	assertTemplateSpecImmutability(t,
		&webhooks.TinkerbellMachineTemplate{},
		func(marker string) *v1beta2.TinkerbellMachineTemplate {
			return &v1beta2.TinkerbellMachineTemplate{
				Spec: v1beta2.TinkerbellMachineTemplateSpec{
					Template: v1beta2.TinkerbellMachineTemplateResource{
						Spec: v1beta2.TinkerbellMachineConfig{TemplateInline: marker},
					},
				},
			}
		},
		func(obj *v1beta2.TinkerbellMachineTemplate, labels map[string]string) {
			obj.Spec.Template.ObjectMeta = clusterv1.ObjectMeta{Labels: labels}
		},
	)
}
