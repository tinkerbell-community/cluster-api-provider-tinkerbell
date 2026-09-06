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

package v1beta2_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	infrav2 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// TestAddToSchemeRegistersAllKinds guards against a new API type being added to the package
// without being wired into the SchemeBuilder, which would only surface at manager start-up.
func TestAddToSchemeRegistersAllKinds(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := infrav2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{
		"TinkerbellCluster",
		"TinkerbellClusterList",
		"TinkerbellClusterTemplate",
		"TinkerbellClusterTemplateList",
		"TinkerbellMachine",
		"TinkerbellMachineList",
		"TinkerbellMachineTemplate",
		"TinkerbellMachineTemplateList",
	} {
		gvk := infrav2.GroupVersion.WithKind(kind)
		if _, err := scheme.New(gvk); err != nil {
			t.Errorf("scheme.New(%s): %v", gvk, err)
		}
	}
}
